package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"gopkg.in/yaml.v2"
	"html/template"
	"io/ioutil"
	"log"
	"log/syslog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	irc "github.com/fluffle/goirc/client"
)

const CONN_RETRY_DELAY = 3

type Config struct {
	UseSyslog      bool   `yaml:"useSyslog"`
	Nick           string `yaml:"nick"`
	NickPassword   string `yaml:"nickPassword"`
	IrcAddress     string `yaml:"ircAddress"`
	IrcSSL         bool   `yaml:"ircSSL"`
	DefaultChannel string `yaml:"defaultChannel"`
	HttpAddress    string `yaml:"httpAddress"`
	HttpURL        string `yaml:"httpURL"`
}

// The struct going from the HTTP go routine to the IRC channel by the Bridge chan
type ChannelMessage struct {
	Channel string
	Message []byte
}

type IRCBridge struct {
	Client *irc.Conn
	Bridge chan ChannelMessage
	config *Config
}

func (irc *IRCBridge) Channels() []string {
	cs := make([]string, 0)
	for ch := range irc.Client.Me().Channels {
		cs = append(cs, ch)
	}
	return cs
}

// goroutine blocking on receiving messages and emitting them to the appropriate chan
func (irc *IRCBridge) recv() {
	for {
		msg := <-irc.Bridge
		channel := fmt.Sprintf("#%s", msg.Channel)
		for _, line := range bytes.Split(msg.Message, []byte("\n")) {
			strMsg := fmt.Sprintf("%s", line)
			irc.Emit(channel, strMsg)
		}
	}
}

func (irc *IRCBridge) Emit(channel string, message string) {
	// join channels we don't track
	if _, isOn := irc.Client.StateTracker().IsOn(channel, irc.Client.Me().Nick); !isOn {
		log.Println("Joining", channel)
		irc.Client.Join(channel)
	}
	irc.Client.Privmsg(channel, message)
}

func (irc *IRCBridge) ReceiveHTTPMessage(w http.ResponseWriter, r *http.Request, channel string) {
	var msg []byte
	if r.Method != "POST" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ct := r.Header.Get("Content-Type")
	if ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data" {
		msg = []byte(strings.TrimSpace(r.FormValue("msg")))
		if len(msg) == 0 {
			return
		}

	} else {
		var err error
		msg, err = ioutil.ReadAll(r.Body)
		if err != nil {
			fmt.Fprintf(w, "POST error in body reading: %s", err)
			return
		}
	}

	// Can't acknowledge this message
	if !irc.Client.Connected() {
		log.Printf("Couldn't send '%s' to channel %s on behalf of %s",
			bytes.Replace(msg, []byte("\n"), []byte("\\n"), -1),
			channel,
			r.RemoteAddr)
		w.Header().Set("Retry-After", fmt.Sprintf("%d", CONN_RETRY_DELAY*2))
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	log.Printf("%s sent '%s' to channel %s",
		r.RemoteAddr,
		bytes.Replace(msg, []byte("\n"), []byte("\\n"), -1),
		channel)
	irc.Bridge <- ChannelMessage{channel, msg}
}

func (irc *IRCBridge) connectRetry() {
	for err := irc.connect(); err != nil; {
		time.Sleep(CONN_RETRY_DELAY * time.Second)
		err = irc.connect()
	}
}

func (irc *IRCBridge) connect() (err error) {
	log.Printf("Connecting to IRC %s", irc.config.IrcAddress)

	irc.Client.Config().Server = irc.config.IrcAddress
	if irc.config.IrcSSL {
		irc.Client.Config().SSL = true
		irc.Client.Config().SSLConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if err = irc.Client.Connect(); err != nil {
		log.Printf("Connection error: %s\n", err)
	}
	return
}

func loadConfig() *Config {
	config := &Config{
		UseSyslog:      false,
		Nick:           "parrot",
		IrcAddress:     "irc.freenode.net",
		IrcSSL:         true,
		DefaultChannel: "parrot",
		HttpAddress:    ":5555",
	}

	if len(os.Args) < 2 {
		log.Fatal("Missing config file")
	}

	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	decoder := yaml.NewDecoder(f)
	if err := decoder.Decode(config); err != nil {
		log.Fatal(err)
	}

	return config
}

func main() {
	config := loadConfig()

	if config.HttpURL == "" {
		log.Fatal("Please specify the HTTP URL for the end user to use httpUrl:...")
	}

	if config.UseSyslog {
		sl, err := syslog.New(syslog.LOG_INFO, "parrot")
		if err != nil {
			log.Fatalf("Can't initialize syslog: %v", err)
		}
		log.SetOutput(sl)
		log.SetFlags(0)
	}

	filename := "home.html"
	t, err := template.ParseFiles(filename)
	if err != nil {
		panic(err)
	}

	// create new IRC connection
	c := irc.SimpleClient(config.Nick, config.Nick)

	parrot := IRCBridge{c, make(chan ChannelMessage), config}

	// keep track of channels we're on (and much more we don't need)
	c.EnableStateTracking()

	c.HandleFunc("connected",
		func(conn *irc.Conn, line *irc.Line) {
			conn.Join(fmt.Sprintf("#%s", config.DefaultChannel))
			log.Printf("Connected")
			if len(config.NickPassword) > 0 {
				conn.Privmsg("NickServ", "IDENTIFY "+config.NickPassword)
			}
		})
	c.HandleFunc("disconnected",
		func(conn *irc.Conn, line *irc.Line) {
			conn.Join(fmt.Sprintf("#%s", config.DefaultChannel))
			log.Printf("Oops got disconnected, retrying to connect...")
			go parrot.connectRetry()
		})

	c.HandleFunc("NOTICE",
		func(conn *irc.Conn, line *irc.Line) {
			log.Printf("NOTICE: %s", line.Raw)
		})

	c.HandleFunc("PRIVMSG",
		func(conn *irc.Conn, line *irc.Line) {
			channel := line.Args[0]
			message := line.Args[1]
			standardDisclaimer := fmt.Sprintf("I'm not very smart, see %s", config.HttpURL)
			r, err := regexp.Compile(fmt.Sprintf("(?i:%s|%s|parrot)(?::|,)",
				regexp.QuoteMeta(config.Nick),
				regexp.QuoteMeta(conn.Me().Nick)))
			if err != nil {
				log.Printf("err: %s, %s\n", conn.Me().Nick, err)
				return
			}
			if channel == conn.Me().Nick {
				log.Printf("%s said to me %s: %s\n", line.Nick, channel, message)
				conn.Privmsg(line.Nick, standardDisclaimer)
			} else if r.MatchString(message) {
				log.Printf("%s said to me %s: %s\n", line.Nick, channel, message)
				conn.Privmsg(channel, standardDisclaimer)
			}
		})

	// Print a small SYNOPSIS on home page of the HTTP server
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		home := struct {
			Nick        string
			Channels    []string
			Url         string
			HttpAddress string
			IrcAddress  string
		}{
			parrot.Client.Me().Nick,
			parrot.Channels(),
			config.HttpURL,
			config.HttpAddress,
			parrot.config.IrcAddress,
		}
		t.Execute(w, home)
	})

	// Message handlers
	http.HandleFunc("/post/", func(w http.ResponseWriter, r *http.Request) {
		lenPath := len("/post/")
		channel := r.URL.Path[lenPath:]
		if len(strings.TrimSpace(channel)) == 0 {
			channel = config.DefaultChannel
		}
		parrot.ReceiveHTTPMessage(w, r, channel)
	})
	http.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
		parrot.ReceiveHTTPMessage(w, r, config.DefaultChannel)
	})

	// start receiver
	go parrot.recv()

	// connect to irc server
	ircerr := parrot.connect()
	if ircerr != nil {
		os.Exit(1)
	}

	log.Printf("HTTP server running at %s", config.HttpAddress)
	httpErr := http.ListenAndServe(config.HttpAddress, nil)
	if httpErr != nil {
		log.Fatalf("HTTP error: %s", httpErr)
	}
}
