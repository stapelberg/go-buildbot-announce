// vim:ts=4:sw=4
// i3 - IRC bot to announce buildbot status and commits
// also fetches the HTML <title> of URLs and posts links to i3 documentation
// upon keywords like >userguide
// © 2011-2012 Michael Stapelberg (see also: LICENSE)
package main

import irc "github.com/fluffle/goirc/client"
import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var to_irc chan string

var irc_channel *string = flag.String("channel", "#i3",
	"In which channel this bot should be in")

// This is naive, but hopefully good enough :)
var url_re *regexp.Regexp = regexp.MustCompile("(http://(?:[^ ]*))")

// Another simple HTML parsing regular expression, but since we control the
// output (served by cgit), that’s not a big problem :).
var doclink_re *regexp.Regexp = regexp.MustCompile(`href='[^']*'>([^<]*)\.html`)
var docref_re *regexp.Regexp = regexp.MustCompile(`\s*>([a-zA-Z0-9-]*)(#[a-zA-Z0-9_-]+)?\b`)

// List of documentation filenames, without the trailing .html, so for example
// "userguide", "multi-monitor", etc.
var docfiles []string

// Helper type: We first unmarshal the JSON into this type to get access to the
// "event" string, then we decide which concrete type to unmarshal to.
type minimalBuildbotEvent struct {
	Event string
}

// Every event type has to implement this interface so that we can print it to
// IRC.
type BuildbotPrintableEvent interface {
	AsChatLine() string
}

// A generic type to unmarshal to. Ev will either be set to some object
// conforming to BuildbotPrintableEvent or nil.
type BuildbotEvent struct {
	Ev BuildbotPrintableEvent
}

// Type corresponding to the buildFinished event which buildbot sends.
type BuildFinishedEvent struct {
	Payload struct {
		Build struct {
			// We have to use the empty interface here because buildbot sends
			// JSON values of varying type. They can be either string, number
			// or null.
			Properties [][]interface{}
		}
	}

	// Properties which are saved in StoreKeyValue and assembled to an IRC line
	// in AsChatLine.
	buildername string
	gitversion  string
	ircsuffix   string
}

func (o *BuildFinishedEvent) AsChatLine() string {
	return fmt.Sprintf("%s finished for %s%s",
		o.buildername, o.gitversion, o.ircsuffix)
}

func (o *BuildFinishedEvent) StoreKeyValue(key, value string) {
	switch key {
	case "buildername":
		o.buildername = value
	case "gitversion":
		o.gitversion = value
	case "ircsuffix":
		o.ircsuffix = value
	}
}

func (o *BuildbotEvent) UnmarshalJSON(data []byte) error {
	var intermediate minimalBuildbotEvent
	if err := json.Unmarshal(data, &intermediate); err != nil {
		return err
	}
	if intermediate.Event == "buildFinished" {
		event := new(BuildFinishedEvent)
		if err := json.Unmarshal(data, event); err != nil {
			return err
		}

		for _, property := range event.Payload.Build.Properties {
			// Every property consists of a triple: key, value, something
			if len(property) != 3 {
				continue
			}

			// Try to convert key and value to string. If that fails, the
			// property might still be valid, but we are only interested in the
			// strings :).
			key, key_ok := property[0].(string)
			value, value_ok := property[1].(string)
			if key_ok && value_ok {
				event.StoreKeyValue(key, value)
			}
		}

		o.Ev = event
	}
	return nil
}

func getURLTitle(url string) {
	result := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			result <- nil
			return
		}
		result <- resp
	}()

	go func() {
		time.Sleep(10 * time.Second)
		result <- nil
	}()

	resp := <-result
	if resp == nil {
		return
	}
	defer resp.Body.Close()

	fmt.Printf(`URL "%s", status %d\n`, url, resp.StatusCode)

	// Check for the special case of a , or ) being the last character of the
	// URL. This happens when the URL is used without leaving a whitespace
	// between the text, for example in "hey, i followed the userguide
	// (http://i3wm.org/docs/userguide.html) and it doesn’t work". We can’t
	// always split on these characters since some pages (like spiegel.de) use
	// strange characters in their normal URLs.
	if resp.StatusCode == 404 &&
		(strings.HasSuffix(url, ",") || strings.HasSuffix(url, ")")) {
		getURLTitle(strings.TrimRight(url, ",)"))
		return
	}

	if resp.StatusCode != 200 {
		return
	}

	reader := bufio.NewReaderSize(resp.Body, 1*1024*1024)
	for {
		line, _, readerr := reader.ReadLine()
		if readerr != nil {
			fmt.Printf("read error (HTTP response for %s): %s\n", url, readerr.Error())
			return
		}
		titleRegexp := regexp.MustCompile("<title>(.*)</title>")
		matches := titleRegexp.FindSubmatch(line)
		if len(matches) > 1 {
			to_irc <- fmt.Sprintf("[Link info] %s", string(matches[1]))
			return
		}

		if readerr != nil {
			log.Printf("Error reading HTTP response for %s: %s\n", url, readerr.Error())
			return
		}
	}
}

func handleLine(conn *irc.Conn, line *irc.Line) {
	msg := line.Args[1]
	if line.Args[0] != *irc_channel {
		log.Printf(`Ignoring private message to me: "%s"`, msg)
		return
	}

	// We have a few trigger words which aim to make support easier:
	docmatches := docref_re.FindAllStringSubmatch(msg, -1)
	for _, match := range docmatches {
		docref := strings.ToLower(match[1])
		log.Printf("Checking whether *%s* is a valid docref…", docref)
		for _, valid_doc := range docfiles {
			if valid_doc == match[1] {
				if len(match) > 2 {
					to_irc <- fmt.Sprintf("[Documentation reference] http://i3wm.org/docs/%s.html%s", match[1], match[2])
				} else {
					to_irc <- fmt.Sprintf("[Documentation reference] http://i3wm.org/docs/%s.html", match[1])
				}
				break
			}
		}
	}
}

// Gets the directory index of
// http://code.stapelberg.de/git/i3-website/tree/docs and stores all .html
// files in a list so that we can recognize them in IRC messages.
func getDocFilenames() {
	log.Println("Retrieving documentation index…")
	resp, err := http.Get("http://code.stapelberg.de/git/i3-website/tree/docs")
	if err != nil {
		log.Printf("Could not get documentation index: %v", err)
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Could not read documentation index response: %v", err)
		return
	}

	docfiles = []string{}
	matches := doclink_re.FindAllStringSubmatch(string(body), -1)
	for _, match := range matches {
		docfiles = append(docfiles, match[1])
	}

	log.Printf("docfiles = %s", docfiles)
}

func main() {
	flag.Parse()

	go getDocFilenames()

	// Channel on which the HTTP handler sends lines to IRC.
	to_irc = make(chan string)
	quit := make(chan bool)

	http.HandleFunc("/push_buildbot",
		func(w http.ResponseWriter, r *http.Request) {
			// Buildbot sends the packets URL-encoded.
			if err := r.ParseForm(); err != nil {
				log.Printf("Could not ParseForm: %s", err.Error())
			}

			// Decode the JSON into BuildbotEvents and send them to IRC if
			// appropriate.
			var packets []BuildbotEvent
			err := json.Unmarshal([]byte(r.Form.Get("packets")), &packets)
			if err != nil {
				log.Printf("Could not parse JSON: %s\n", err.Error())
			}
			for _, event := range packets {
				if event.Ev != nil {
					to_irc <- event.Ev.AsChatLine()
				}
			}
		})

	http.HandleFunc("/push_commit",
		func(w http.ResponseWriter, r *http.Request) {
			body, err := ioutil.ReadAll(r.Body)
			if err != nil {
				log.Printf("Could not read body: %s\n", err.Error())
				return
			}
			lines := strings.Split(string(body), "\n")
			for _, line := range lines {
				to_irc <- line
			}
		})

	// Handle HTTP requests in a different Goroutine.
	go func() {
		if err := http.ListenAndServe("localhost:8080", nil); err != nil {
			log.Fatal("ListenAndServer: ", err.Error())
		}
	}()

	c := irc.SimpleClient("i3", "i3", "http://build.i3wm.org/")

	c.AddHandler("connected",
		func(conn *irc.Conn, line *irc.Line) {
			log.Printf("Connected, joining channel %s\n", *irc_channel)
			conn.Join(*irc_channel)
		})

	c.AddHandler("disconnected",
		func(conn *irc.Conn, line *irc.Line) { quit <- true })

	c.AddHandler("PRIVMSG", handleLine)

	log.Printf("Connecting...\n")
	if err := c.Connect("irc.twice-irc.de"); err != nil {
		log.Printf("Connection error: %s\n", err.Error())
	}

	everyDay := make(chan bool)
	go func() {
		for {
			time.Sleep(24 * time.Hour)
			everyDay <- true
		}
	}()

	// program main loop
	for {
		select {
		case line, _ := <-to_irc:
			c.Privmsg(*irc_channel, line)
		case <-everyDay:
			go getDocFilenames()
		case <-quit:
			log.Println("Disconnected. Reconnecting...")
			if err := c.Connect("irc.twice-irc.de"); err != nil {
				log.Printf("Connection error: %s\n", err.Error())
			}
		}
	}
	log.Fatalln("Fell out of the main loop?!")
}
