package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// STRUCTS

type profile struct {
	Source        string              `yaml:"source"`
	Public        bool                `yaml:"public"`
	ImmutablePast bool                `yaml:"immutable-past,omitempty"`
	Tokens        []string            `yaml:"admin-tokens"`
	Modules       []map[string]string `yaml:"modules,omitempty"`
}

type mailConfig struct {
	SMTPServer string `yaml:"smtp_server"`
	SMTPPort   int    `yaml:"smtp_port"`
	Sender     string `yaml:"sender"`
	SMTPUser   string `yaml:"smtp_user,omitempty"`
	SMTPPass   string `yaml:"smtp_pass,omitempty"`
}

type serverConfig struct {
	Addr          string     `yaml:"addr"`
	URL           string     `yaml:"url"`
	LogLevel      log.Level  `yaml:"loglevel"`
	StoragePath   string     `yaml:"storagepath"`
	TemplatePath  string     `yaml:"templatepath"`
	Imprint       string     `yaml:"imprintlink"`
	PrivacyPolicy string     `yaml:"privacypolicylink"`
	Mail          mailConfig `yaml:"mail,omitempty"`
	SuperTokens   []string   `yaml:"super-tokens,omitempty"`
}

type notifier struct {
	Source     string   `yaml:"source"`
	Interval   string   `yaml:"interval"`
	Recipients []string `yaml:"recipients"`
}

// Config represents configuration for the application
type Config struct {
	Server    serverConfig        `yaml:"server"`
	Profiles  map[string]profile  `yaml:"profiles,omitempty"`
	Notifiers map[string]notifier `yaml:"notifiers,omitempty"`
}

// CONFIG MANAGEMENT FUNCTIONS

// ParseConfig reads config from path and returns a Config struct
func ParseConfig(path string) (Config, error) {
	var tmpConfig Config

	yamlFile, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatalf("Error Reading Config: %v ", err)
		return tmpConfig, err
	}

	err = yaml.Unmarshal(yamlFile, &tmpConfig)
	if err != nil {
		log.Fatalf("Error Unmarshalling Config: %v", err)
		return tmpConfig, err
	}

	// defaults
	if tmpConfig.Server.Addr == "" {
		tmpConfig.Server.Addr = ":8080"
	}
	if tmpConfig.Server.LogLevel == 0 {
		tmpConfig.Server.LogLevel = log.InfoLevel
	}
	if tmpConfig.Server.StoragePath == "" {
		tmpConfig.Server.StoragePath = filepath.Dir(path)
	}
	if !strings.HasSuffix(tmpConfig.Server.StoragePath, "/") {
		tmpConfig.Server.StoragePath += "/"
	}
	if tmpConfig.Server.TemplatePath == "" {
		tmpConfig.Server.TemplatePath = filepath.Dir("/opt/ical-relay/templates/")
	}
	if !strings.HasSuffix(tmpConfig.Server.TemplatePath, "/") {
		tmpConfig.Server.TemplatePath += "/"
	}

	if !directoryExists(tmpConfig.Server.StoragePath + "notifystore/") {
		log.Info("Creating notifystore directory")
		err = os.MkdirAll(tmpConfig.Server.StoragePath+"notifystore/", 0750)
		if err != nil {
			log.Fatalf("Error creating notifystore: %v", err)
			return tmpConfig, err
		}
	}
	if !directoryExists(tmpConfig.Server.StoragePath + "calstore/") {
		log.Info("Creating calstore directory")
		err = os.MkdirAll(tmpConfig.Server.StoragePath+"calstore/", 0750)
		if err != nil {
			log.Fatalf("Error creating calstore: %v", err)
			return tmpConfig, err
		}
	}

	return tmpConfig, nil
}

func reloadConfig() error {
	// load config
	var err error
	conf, err = ParseConfig(configPath)
	if err != nil {
		return err
	} else {
		log.Info("Config reloaded")
		return nil
	}
}

func (c Config) saveConfig(path string) error {
	d, err := yaml.Marshal(&c)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path, d, 0600)
}

// CONFIG EDITING FUNCTIONS

func (c Config) getPublicCalendars() []string {
	var cal []string
	for p := range c.Profiles {
		if c.Profiles[p].Public {
			cal = append(cal, p)
		}
	}
	return cal
}

func (c Config) profileExists(name string) bool {
	_, ok := c.Profiles[name]
	return ok
}

func (c Config) notifierExists(name string) bool {
	_, ok := c.Notifiers[name]
	return ok
}

func (c Config) addNotifierFromProfile(name string) {
	c.Notifiers[name] = notifier{
		Source:     c.Server.URL + "/profiles/" + name,
		Interval:   "1h",
		Recipients: []string{},
	}
}

func (c Config) addNotifyRecipient(notifier string, recipient string) error {
	if _, ok := c.Notifiers[notifier]; ok {
		n := c.Notifiers[notifier]
		n.Recipients = append(n.Recipients, recipient)
		c.Notifiers[notifier] = n
		return c.saveConfig(configPath)
	} else {
		return fmt.Errorf("notifier does not exist")
	}
}

func (c Config) removeNotifyRecipient(notifier string, recipient string) error {
	if _, ok := c.Notifiers[notifier]; ok {
		n := c.Notifiers[notifier]
		for i, r := range n.Recipients {
			if r == recipient {
				n.Recipients = append(n.Recipients[:i], n.Recipients[i+1:]...)
				c.Notifiers[notifier] = n
				return c.saveConfig(configPath)
			}
		}
		return fmt.Errorf("recipient not found")
	} else {
		return fmt.Errorf("notifier does not exist")
	}
}

func (c Config) addModule(profile string, module map[string]string) error {
	if !c.profileExists(profile) {
		return fmt.Errorf("profile " + profile + " does not exist")
	}
	p := c.Profiles[profile]
	p.Modules = append(c.Profiles[profile].Modules, module)
	c.Profiles[profile] = p
	return c.saveConfig(configPath)
}

func (c Config) removeModuleFromProfile(profile string, index int) {
	log.Info("Removing expired module at position " + fmt.Sprint(index+1) + " from profile " + profile)
	removeFromMapString(c.Profiles[profile].Modules, index)
	c.saveConfig(configPath)
}

func (c Config) RunCleanup() {
	for p := range c.Profiles {
		for i, m := range c.Profiles[p].Modules {
			if m["expires"] != "" {
				exp, _ := time.Parse(time.RFC3339, m["expiration"])
				if time.Now().After(exp) {
					c.removeModuleFromProfile(p, i)
				}
			}
		}
	}
}

// starts a heartbeat notifier in a sub-routine
func CleanupStartup() {
	log.Info("Starting Cleanup Timer")
	go TimeCleanup()
}

func TimeCleanup() {
	interval, _ := time.ParseDuration("1h")
	if interval == 0 {
		// failsave for 0s interval, to make machine still responsive
		interval = 1 * time.Second
	}
	log.Debug("Cleanup Timer, Interval: " + interval.String())
	// endless loop
	for {
		time.Sleep(interval)
		conf.RunCleanup()
	}
}
