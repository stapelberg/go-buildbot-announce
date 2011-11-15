// vim:ts=4:sw=4
// i3build - IRC bot to announce buildbot status
// © 2011 Michael Stapelberg (see also: LICENSE)
package main

import irc "github.com/fluffle/goirc/client"
import (
	"fmt"
	"http"
	"log"
	"json"
	"os"
	"flag"
)

var irc_channel *string = flag.String("channel", "#i3",
	"In which channel this bot should be in")

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

func (o *BuildbotEvent) UnmarshalJSON(data []byte) os.Error {
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

func main() {
	flag.Parse()

	// Channel on which the HTTP handler sends lines to IRC.
	to_irc := make(chan string)
	quit := make(chan bool)

	http.HandleFunc("/push_buildbot",
		func(w http.ResponseWriter, r *http.Request) {
			// Buildbot sends the packets URL-encoded.
			if err := r.ParseForm(); err != nil {
				log.Printf("Could not ParseForm: %s", err.String())
			}

			// Decode the JSON into BuildbotEvents and send them to IRC if
			// appropriate.
			var packets []BuildbotEvent
			err := json.Unmarshal([]byte(r.Form.Get("packets")), &packets)
			if err != nil {
				log.Printf("Could not parse JSON: %s\n", err.String())
			}
			for _, event := range packets {
				if event.Ev != nil {
					to_irc <- event.Ev.AsChatLine()
				}
			}
		})

	// Handle HTTP requests in a different Goroutine.
	go func() {
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatal("ListenAndServer: ", err.String())
		}
	}()

	c := irc.SimpleClient("i3build", "i3build", "http://build.i3wm.org/")

	c.AddHandler("connected",
		func(conn *irc.Conn, line *irc.Line) {
			log.Printf("Connected, joining channel %s\n", *irc_channel)
			conn.Join(*irc_channel)
		})

	c.AddHandler("disconnected",
		func(conn *irc.Conn, line *irc.Line) { quit <- true })

	log.Printf("Connecting...\n")
	if err := c.Connect("irc.twice-irc.de"); err != nil {
		log.Printf("Connection error: %s\n", err.String())
	}

	// program main loop
	for {
		select {
		case line, _ := <-to_irc:
			c.Privmsg(*irc_channel, line)
		case <-quit:
			log.Println("Disconnected. Reconnecting...")
			if err := c.Connect("irc.twice-irc.de"); err != nil {
				log.Printf("Connection error: %s\n", err.String())
			}
		}
	}
	log.Fatalln("Fell out of the main loop?!")
}
