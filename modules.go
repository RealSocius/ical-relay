package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
	log "github.com/sirupsen/logrus"
)

var modules = map[string]func(*ics.Calendar, map[string]string) (int, error){
	"delete-bysummary-regex": moduleDeleteSummaryRegex,
	"delete-byid":            moduleDeleteId,
	"add-url":                moduleAddURL,
	"add-file":               moduleAddFile,
	"delete-timeframe":       moduleDeleteTimeframe,
	"delete-duplicates":      moduleDeleteDuplicates,
	"edit-byid":              moduleEditId,
	"edit-bysummary-regex":   moduleEditSummaryRegex,
	"save-to-file":           moduleSaveToFile,
	"add-reminder":           moduleAddAllReminder,
}

// These modules are allowed to be edited by the module admin. This is a security measure to prevent SSRF and LFI attacks.
var lowPrivModules = []string{
	"delete-bysummary-regex",
	"delete-byid",
	"delete-timeframe",
	"delete-duplicates",
	"edit-byid",
	"edit-bysummary-regex",
}

// This wrappter gets a function from the above modules map and calls it with the parameters and the passed calendar.
// parameters can be any dictionary. The function will then choose how to handle the parameters.
// Returns the number of added entries. negative, if it removed entries.
func callModule(module func(*ics.Calendar, map[string]string) (int, error), params map[string]string, cal *ics.Calendar) (int, error) {
	return module(cal, params)
}

// This modules delete all events whose summary match the regex and are in the time range from the calendar.
// Parameters:
//   - 'regex', mandatory: regular expression to remove.
//   - 'from' & 'until', optional parameters: If timeframe is not given, all events matching the regex are removed.
//     Currenty if only either "from" or "until" is set, the timeframe will be ignored. TODO
//
// Returns the number of events removed. This number should always be negative.
func moduleDeleteSummaryRegex(cal *ics.Calendar, params map[string]string) (int, error) {
	var count int
	if params["regex"] == "" {
		return 0, fmt.Errorf("missing mandatory Parameter 'regex'")
	}
	regex, _ := regexp.Compile(params["regex"])
	if params["from"] != "" && params["until"] != "" {
		from, _ := time.Parse(time.RFC3339, params["from"])
		until, _ := time.Parse(time.RFC3339, params["until"])
		count = removeByRegexSummaryAndTime(cal, *regex, from, until)
	} else {
		count = removeByRegexSummary(cal, *regex)
	}
	if count > 0 {
		return count, fmt.Errorf("this number should not be positive")
	}
	return count, nil
}

// This function is a wrapper for removeByRegexSummaryAndTime, where the time is any time
func removeByRegexSummary(cal *ics.Calendar, regex regexp.Regexp) int {
	return removeByRegexSummaryAndTime(cal, regex, time.Time{}, time.Unix(1<<63-1-int64((1969*365+1969/4-1969/100+1969/400)*24*60*60), 999999999))
	// this is the maximum time that can be represented in the time.Time struct
}

// This function is used to remove the events that are in the time range and match the regex string.
// It returns the number of events removed. (always negative)
func removeByRegexSummaryAndTime(cal *ics.Calendar, regex regexp.Regexp, start time.Time, end time.Time) int {
	var count int
	for i := len(cal.Components) - 1; i >= 0; i-- { // iterate over events
		switch cal.Components[i].(type) {
		case *ics.VEvent:
			event := cal.Components[i].(*ics.VEvent)
			date, _ := event.GetStartAt()
			if date.After(start) && end.After(date) {
				// event is in time range
				if regex.MatchString(event.GetProperty(ics.ComponentPropertySummary).Value) {
					// event matches regex
					cal.Components = removeFromICS(cal.Components, i)
					log.Debug("Excluding event '" + event.GetProperty(ics.ComponentPropertySummary).Value + "' with id " + event.Id() + "\n")
					count--
				}
			}
		default:
			// print type of component
			log.Debug("Unknown component type ignored: " + reflect.TypeOf(cal.Components[i]).String() + "\n")
		}
	}
	return count
}

// This module deletes an Event with the given id.
// Parameters: "id" mandatory
// Returns the number of events removed.
func moduleDeleteId(cal *ics.Calendar, params map[string]string) (int, error) {
	var count int
	if params["id"] == "" {
		return 0, fmt.Errorf("missing mandatory Parameter 'id'")
	}
	for i, component := range cal.Components { // iterate over events
		switch component.(type) {
		case *ics.VEvent:
			event := component.(*ics.VEvent)
			if event.Id() == params["id"] {
				cal.Components = removeFromICS(cal.Components, i)
				count--
				log.Debug("Excluding event with id " + params["id"] + "\n")
				break
			}
		}
	}
	return count, nil
}

// This function adds all events from cal2 to cal1.
// All other properties, such as TZ are retained from cal1.
func addEvents(cal1 *ics.Calendar, cal2 *ics.Calendar) int {
	var count int
	for _, event := range cal2.Events() {
		cal1.AddVEvent(event)
		count++
	}
	return count
}

func moduleAddURL(cal *ics.Calendar, params map[string]string) (int, error) {
	if params["url"] == "" {
		return 0, fmt.Errorf("missing mandatory Parameter 'url'")
	}
	// put all params starting with header- into header map
	header := make(map[string]string)
	for k, v := range params {
		if strings.HasPrefix(k, "header-") {
			header[strings.TrimPrefix(k, "header-")] = v
		}
	}

	return addEventsURL(cal, params["url"], header)
}

func addEventsURL(cal *ics.Calendar, url string, headers map[string]string) (int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response, err := http.DefaultClient.Do(req)

	if err != nil {
		log.Errorln(err)
		return 0, fmt.Errorf("error requesting additional URL: %s", err.Error())
	}
	if response.StatusCode != 200 {
		log.Warnf("Unexpected status '%s' from additional URL '%s'", response.Status, url)
		resp, err := ioutil.ReadAll(response.Body)
		if err != nil {
			log.Errorln(err)
		}
		log.Debugf("Full response body: %s\n", resp)
		return 0, nil // we are not returning an error here, to just ignore URLs that are unavailible. TODO: make this configurable
	}
	// parse aditional calendar
	addcal, err := ics.ParseCalendar(response.Body)
	if err != nil {
		log.Errorln(err)
	}
	// add to new calendar
	return addEvents(cal, addcal), nil
}

// This module saves the current calendar to a file.
// Parameters: "file" mandatory: full path of file to save
func moduleSaveToFile(cal *ics.Calendar, params map[string]string) (int, error) {
	if params["file"] == "" {
		return 0, fmt.Errorf("missing mandatory Parameter 'file'")
	}
	err := ioutil.WriteFile(params["file"], []byte(cal.Serialize()), 0600)
	if err != nil {
		log.Errorln(err)
		return 0, fmt.Errorf("error writing to file: %s", err.Error())
	}
	return 0, nil
}

func addMultiURL(cal *ics.Calendar, urls []string, header map[string]string) (int, error) {
	var count int
	for _, url := range urls {
		c, err := addEventsURL(cal, url, header)
		if err != nil {
			return count, err
		}
		count += c
	}
	return count, nil
}

func moduleAddFile(cal *ics.Calendar, params map[string]string) (int, error) {
	if params["filename"] == "" {
		return 0, fmt.Errorf("missing mandatory Parameter 'filename'")
	}
	return addEventsFile(cal, params["filename"])
}

func addEventsFile(cal *ics.Calendar, filename string) (int, error) {
	if _, err := os.Stat(filename); err != nil {
		return 0, fmt.Errorf("file %s not found", filename)
	}
	addicsfile, _ := os.Open(filename)
	addics, _ := ics.ParseCalendar(addicsfile)
	return addEvents(cal, addics), nil
}

func addMultiFile(cal *ics.Calendar, filenames []string) (int, error) {
	var count int
	for _, filename := range filenames {
		c, err := addEventsFile(cal, filename)
		if err != nil {
			return count, err
		}
		count += c
	}
	return count, nil
}

// Removes all Events in a passed Timeframe.
// Sets UNTIL parameter to the end of the timeframe for RRULE events.
// Parameters: either "after" or "before" mandatory
// Format is RFC3339: "2006-01-02T15:04:05Z"
// or "now" for current time
// Returns the number of events removed. (always negative)
func moduleDeleteTimeframe(cal *ics.Calendar, params map[string]string) (int, error) {
	var count int
	var after time.Time
	var before time.Time
	var err error
	if params["after"] == "" && params["before"] == "" {
		return 0, fmt.Errorf("missing both Parameters 'start' or 'end'. One has to be present")
	}
	if params["after"] == "" {
		log.Debug("No after time given. Using time 0.\n")
		after = time.Time{}
	} else if params["after"] == "now" {
		after = time.Now()
	} else {
		after, err = time.Parse(time.RFC3339, params["after"])
		if err != nil {
			return 0, fmt.Errorf("invalid start time: %s", err.Error())
		}
	}
	if params["before"] == "" {
		log.Debug("No end time given. Using max time\n")
		before = time.Unix(1<<63-1-int64((1969*365+1969/4-1969/100+1969/400)*24*60*60), 999999999)
	} else if params["before"] == "now" {
		before = time.Now()
	} else {
		before, err = time.Parse(time.RFC3339, params["before"])
		if err != nil {
			return 0, fmt.Errorf("invalid end time: %s", err.Error())
		}
	}

	log.Debugf("Deleting events between %s and %s\n", after.Format(time.RFC3339), before.Format(time.RFC3339))
	// remove events
	for i := len(cal.Components) - 1; i >= 0; i-- { // iterate over events
		switch cal.Components[i].(type) {
		case *ics.VEvent:
			event := cal.Components[i].(*ics.VEvent)
			if event.GetProperty(ics.ComponentPropertyRrule) != nil {
				// event has RRULE
				// TODO handle RRULEs in the past?
				log.Debug("Event with RRULE: " + event.Id())
				// read RRULE, split into different rule parts
				props := strings.Split(event.GetProperty(ics.ComponentPropertyRrule).Value, ";")
				// cast into map for easy queries
				m := make(map[string]string)
				for _, e := range props {
					p := strings.Split(e, "=")
					m[p[0]] = p[1]
				}
				// either UNTIL or COUNT is present, otherwise, add UNTIL
				if v, ok := m["UNTIL"]; ok {
					// time format from from golang-ical/components.go/icalTimestampFormatUtc
					until, err := time.Parse("20060102T150405Z", v)
					if err != nil {
						return 0, fmt.Errorf("invalid UNTIL time: %s", err.Error())
					}
					// only checking after to not break RRULEs with UNTIL in the past
					if until.After(after) {
						// RRULE UNTIL is not in timeframe, shortening UNTIL
						log.Debug("Shortening UNTIL in RRULE of event with id " + event.Id() + "\n")
						m["UNTIL"] = after.Format("20060102T150405Z")
					}
				} else if _, ok := m["COUNT"]; ok {
					// TODO implement calculating COUNT
					log.Debug("COUNT in RRULE of event with id " + event.Id() + " not implemented\n")
				} else {
					// no UNTIL or COUNT, adding UNTIL
					log.Debug("Adding UNTIL in RRULE of event with id " + event.Id() + "\n")
					m["UNTIL"] = after.Format("20060102T150405Z")
				}
				// reassemble RRULE
				rrulestring := ""
				for k, v := range m {
					rrulestring += k + "=" + v + ";"
				}
				// delete old RRULE. TODO upstream function to delete property
				for i, p := range event.Properties {
					if ics.ComponentProperty(p.IANAToken) == ics.ComponentPropertyRrule {
						removeProperty(event.Properties, i)
					}
				}
				event.AddRrule(rrulestring)
				// adding edited event back to calendar
				cal.Components[i] = event
			}
			date, _ := event.GetStartAt()
			if date.After(after) && before.After(date) {
				cal.Components = removeFromICS(cal.Components, i)
				count--
				log.Debug("Excluding event with id " + event.Id() + "\n")
			}
		}
	}

	return count, nil
}

// This Module deletes duplicate Events.
// No Parameters
// Returns the number of events removed. (always negative)
// Duplicates are defined by the same Summary and the same start and end time. Only the event that is latest in the file will be kept.
// TODO smarter: if the description or other components differ, it should combine them
func moduleDeleteDuplicates(cal *ics.Calendar, params map[string]string) (int, error) {
	var count int
	var uniques []string
	for i := len(cal.Components) - 1; i >= 0; i-- { // iterate over events backwards
		switch cal.Components[i].(type) {
		case *ics.VEvent:
			event := cal.Components[i].(*ics.VEvent)
			start, _ := event.GetStartAt()
			end, _ := event.GetEndAt()
			identifier := start.String() + end.String() + event.GetProperty(ics.ComponentPropertySummary).Value
			if stringInSlice(identifier, uniques) {
				cal.Components = removeFromICS(cal.Components, i)
				count--
				log.Debug("Excluding event with id " + event.Id() + "\n")
			} else {
				uniques = append(uniques, identifier)
			}
		}
	}
	return count, nil
}

// Edits an Event with the passed id.
// Parameters:
// - 'id', mandatory: the id of the event to edit
// - 'overwrite', default true: overwrite existing event properties with the new ones. If false, it will be appended to the existing property. Does not apply to 'new-start' and 'new-end'
// - 'new-summary', optional: the new summary
// - 'new-description', optional: the new description
// - 'new-start', optional: the new start time in RFC3339 format "2006-01-02T15:04:05Z"
// - 'new-end', optional: the new end time in RFC3339 format "2006-01-02T15:04:05Z"
// - 'new-location', optional: the new location
// The return value is the number of events removed or added (should always be 0)
func moduleEditId(cal *ics.Calendar, params map[string]string) (int, error) {
	if params["id"] == "" {
		return 0, fmt.Errorf("missing mandatory Parameter 'id'")
	}
	if params["overwrite"] == "" {
		params["overwrite"] = "true"
	}
	for i := len(cal.Components) - 1; i >= 0; i-- { // iterate over events backwards
		switch cal.Components[i].(type) {
		case *ics.VEvent:
			event := cal.Components[i].(*ics.VEvent)
			if event.Id() == params["id"] {
				log.Debug("Changing event with id " + event.Id())
				if params["new-summary"] != "" {
					if event.GetProperty(ics.ComponentPropertySummary) == nil {
						params["overwrite"] = "true"
						// if the summary is not set, we need to create it
					}
					switch params["overwrite"] {
					case "false":
						event.SetProperty(ics.ComponentPropertySummary, event.GetProperty(ics.ComponentPropertySummary).Value+"; "+params["new-summary"])
					case "fillempty":
						if event.GetProperty(ics.ComponentPropertySummary).Value == "" {
							event.SetProperty(ics.ComponentPropertySummary, params["new-summary"])
						}
					case "true":
						event.SetProperty(ics.ComponentPropertySummary, params["new-summary"])
					}
					log.Debug("Changed summary to " + event.GetProperty(ics.ComponentPropertySummary).Value)
				}
				if params["new-description"] != "" {
					if event.GetProperty(ics.ComponentPropertyDescription) == nil {
						params["overwrite"] = "true"
						// if the description is not set, we need to create it
					}
					switch params["overwrite"] {
					case "false":
						event.SetProperty(ics.ComponentPropertyDescription, event.GetProperty(ics.ComponentPropertyDescription).Value+"; "+params["new-description"])
					case "fillempty":
						if event.GetProperty(ics.ComponentPropertyDescription).Value == "" {
							event.SetProperty(ics.ComponentPropertyDescription, params["new-description"])
						}
					case "true":
						event.SetProperty(ics.ComponentPropertyDescription, params["new-description"])
					}
					log.Debug("Changed description to " + event.GetProperty(ics.ComponentPropertyDescription).Value)
				}
				if params["new-location"] != "" {
					if event.GetProperty(ics.ComponentPropertyLocation) == nil {
						params["overwrite"] = "true"
						// if the description is not set, we need to create it
					}
					switch params["overwrite"] {
					case "false":
						event.SetProperty(ics.ComponentPropertyLocation, event.GetProperty(ics.ComponentPropertyLocation).Value+"; "+params["new-location"])
					case "fillempty":
						if event.GetProperty(ics.ComponentPropertyLocation).Value == "" {
							event.SetProperty(ics.ComponentPropertyLocation, params["new-location"])
						}
					case "true":
						event.SetProperty(ics.ComponentPropertyLocation, params["new-location"])
					}
					log.Debug("Changed location to " + event.GetProperty(ics.ComponentPropertyLocation).Value)
				}
				if params["new-start"] != "" {
					start, err := time.Parse(time.RFC3339, params["new-start"])
					if err != nil {
						return 0, fmt.Errorf("invalid start time: %s", err.Error())
					}
					event.SetStartAt(start)
					log.Debug("Changed start to " + params["new-start"])
				}
				if params["new-end"] != "" {
					end, err := time.Parse(time.RFC3339, params["new-end"])
					if err != nil {
						return 0, fmt.Errorf("invalid end time: %s", err.Error())
					}
					event.SetEndAt(end)
					log.Debug("Changed end to " + params["new-end"])
				}
				// adding edited event back to calendar
				cal.Components[i] = event
				return 0, nil
			}
		}
	}
	log.Debug("No Event with id " + params["id"] + " found")
	return 0, nil
}

// Edits all Events with the matching regex title.
// Parameters:
// - 'id', mandatory: the id of the event to edit
// - 'after', optional: beginning of search timeframe
// - 'before', optional: end of search timeframe
// - 'overwrite', default true: overwrite existing event properties with the new ones. If false, it will be appended to the existing property. Does not apply to 'new-start' and 'new-end'
// - 'new-summary', optional: the new summary
// - 'new-description', optional: the new description
// - 'new-start', optional: the new start time in RFC3339 format "2006-01-02T15:04:05Z"
// - 'new-end', optional: the new end time in RFC3339 format "2006-01-02T15:04:05Z"
// - 'new-location', optional: the new location
// The return value is the number of events removed or added (should always be 0)
func moduleEditSummaryRegex(cal *ics.Calendar, params map[string]string) (int, error) {
	// parse regex
	if params["regex"] == "" {
		return 0, fmt.Errorf("missing mandatory Parameter 'regex'")
	}
	re, err := regexp.Compile(params["regex"])
	if err != nil {
		return 0, fmt.Errorf("invalid regex: %s", err.Error())
	}
	// parse timespan
	var after time.Time
	var before time.Time
	if params["after"] == "" {
		log.Debug("No after time given. Using time 0.\n")
		after = time.Time{}
	} else if params["after"] == "now" {
		after = time.Now()
	} else {
		after, err = time.Parse(time.RFC3339, params["start"])
		if err != nil {
			return 0, fmt.Errorf("invalid start time: %s", err.Error())
		}
	}
	if params["before"] == "" {
		log.Debug("No end time given. Using max time\n")
		before = time.Unix(1<<63-1-int64((1969*365+1969/4-1969/100+1969/400)*24*60*60), 999999999)
	} else if params["before"] == "now" {
		before = time.Now()
	} else {
		before, err = time.Parse(time.RFC3339, params["before"])
		if err != nil {
			return 0, fmt.Errorf("invalid end time: %s", err.Error())
		}
	}

	// parse move-time
	if params["move-time"] != "" && (params["new-start"] != "" || params["new-end"] != "") {
		return 0, fmt.Errorf("two exclusive params were given: 'move-time' and 'new-start'/'new-end'")
	}

	// iterate over events backwards
	for i := len(cal.Components) - 1; i >= 0; i-- {
		switch cal.Components[i].(type) {
		case *ics.VEvent:
			event := cal.Components[i].(*ics.VEvent)
			date, _ := event.GetStartAt()
			if date.After(after) && before.After(date) {
				if re.MatchString(event.GetProperty(ics.ComponentPropertySummary).Value) {
					log.Debug("Changing event with id " + event.Id())
					if params["new-summary"] != "" {
						if event.GetProperty(ics.ComponentPropertySummary) == nil {
							params["overwrite"] = "true"
							// if the summary is not set, we need to create it
						}
						switch params["overwrite"] {
						case "false":
							event.SetProperty(ics.ComponentPropertySummary, event.GetProperty(ics.ComponentPropertySummary).Value+"; "+params["new-summary"])
						case "fillempty":
							if event.GetProperty(ics.ComponentPropertySummary).Value == "" {
								event.SetProperty(ics.ComponentPropertySummary, params["new-summary"])
							}
						case "true":
							event.SetProperty(ics.ComponentPropertySummary, params["new-summary"])
						}
						log.Debug("Changed summary to " + event.GetProperty(ics.ComponentPropertySummary).Value)
					}
					if params["new-description"] != "" {
						if event.GetProperty(ics.ComponentPropertyDescription) == nil {
							params["overwrite"] = "true"
							// if the description is not set, we need to create it
						}
						switch params["overwrite"] {
						case "false":
							event.SetProperty(ics.ComponentPropertyDescription, event.GetProperty(ics.ComponentPropertyDescription).Value+"; "+params["new-description"])
						case "fillempty":
							if event.GetProperty(ics.ComponentPropertyDescription).Value == "" {
								event.SetProperty(ics.ComponentPropertyDescription, params["new-description"])
							}
						case "true":
							event.SetProperty(ics.ComponentPropertyDescription, params["new-description"])
						}
						log.Debug("Changed description to " + event.GetProperty(ics.ComponentPropertyDescription).Value)
					}
					if params["new-location"] != "" {
						if event.GetProperty(ics.ComponentPropertyLocation) == nil {
							params["overwrite"] = "true"
							// if the description is not set, we need to create it
						}
						switch params["overwrite"] {
						case "false":
							event.SetProperty(ics.ComponentPropertyLocation, event.GetProperty(ics.ComponentPropertyLocation).Value+"; "+params["new-location"])
						case "fillempty":
							if event.GetProperty(ics.ComponentPropertyLocation).Value == "" {
								event.SetProperty(ics.ComponentPropertyLocation, params["new-location"])
							}
						case "true":
							event.SetProperty(ics.ComponentPropertyLocation, params["new-location"])
						}
						log.Debug("Changed location to " + event.GetProperty(ics.ComponentPropertyLocation).Value)
					}
					if params["new-start"] != "" {
						start, err := time.Parse(time.RFC3339, params["new-start"])
						if err != nil {
							return 0, fmt.Errorf("invalid start time: %s", err.Error())
						}
						event.SetStartAt(start)
						log.Debug("Changed start to " + params["new-start"])
					}
					if params["new-end"] != "" {
						end, err := time.Parse(time.RFC3339, params["new-end"])
						if err != nil {
							return 0, fmt.Errorf("invalid end time: %s", err.Error())
						}
						event.SetEndAt(end)
						log.Debug("Changed end to " + params["new-end"])
					}
					if params["move-time"] != "" {
						dur, err := time.ParseDuration(params["move-time"])
						if err != nil {
							return 0, fmt.Errorf("invalid duration: %s", err.Error())
						}
						start, _ := event.GetStartAt()
						log.Debug("Starttime is " + start.String())
						end, _ := event.GetEndAt()
						event.SetStartAt(start.Add(dur))
						log.Debug("Changed start to " + start.Add(dur).String())
						event.SetEndAt(end.Add(dur))
						log.Debug("Changed start and end by " + dur.String())
					}
					// adding edited event back to calendar
					cal.Components[i] = event
				}
			}
		}
	}
	return 0, nil
}

func moduleAddAllReminder(cal *ics.Calendar, params map[string]string) (int, error) {
	// add reminder to calendar
	for i := len(cal.Components) - 1; i >= 0; i-- {
		switch cal.Components[i].(type) {
		case *ics.VEvent:
			event := cal.Components[i].(*ics.VEvent)
			event.AddAlarm()
			event.Alarms()[0].SetTrigger(("-PT" + params["time"]))
			event.Alarms()[0].SetAction("DISPLAY")
			cal.Components[i] = event
			log.Debug("Added reminder to event " + event.Id())
		}
	}
	return 0, nil
}

// removes the element at index i from ics.Component slice
// warning: if you iterate over []ics.Component forward, this remove will lead to mistakes. Iterate backwards instead!
func remove(slice []ics.Component, s int) []ics.Component {
	return append(slice[:s], slice[s+1:]...)
}

// removes the element at index i from ics.Component slice
// warning: if you iterate over []ics.IANAProperty forward, this remove will lead to mistakes. Iterate backwards instead!
func removeProperty(slice []ics.IANAProperty, s int) []ics.IANAProperty {
	return append(slice[:s], slice[s+1:]...)
}
