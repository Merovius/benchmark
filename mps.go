// mps is an end-to-end benchmark tool to measure the amount of messages per
// second which can be sent and received through RobustIRC.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/sorcix/irc"
)

var (
	target = flag.String("target",
		"localhost:6667",
		"Target to connect to")

	numSessions = flag.Int("sessions",
		2,
		"Number of sessions to create")

	numMessages = flag.Int("messages",
		1000,
		"Number of messages to send")
)

func main() {
	flag.Parse()

	// TODO(secure): verify that cpu governor is on performance
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	log.Printf("Joining with %d connections, sending %d messages\n",
		*numSessions, *numMessages)

	sessions := make(map[int]*irc.Conn)

	for i := 0; i < *numSessions; i++ {
		rawconn, err := net.Dial("tcp", *target)
		if err != nil {
			log.Fatal(err)
		}
		conn := irc.NewConn(rawconn)
		sessions[i] = conn
		if _, err := conn.Write([]byte(fmt.Sprintf("NICK bench-%d\r\n", i))); err != nil {
			log.Fatal(err)
		}
		if _, err := conn.Write([]byte("JOIN #bench\r\n")); err != nil {
			log.Fatal(err)
		}
		// Wait until we see the RPL_ENDOFNAMES which is the last message that
		// the server generates after a JOIN command.
		for {
			msg, err := conn.Decode()
			if err != nil {
				log.Fatal(err)
			}
			if msg.Command != irc.RPL_ENDOFNAMES {
				continue
			}
			break
		}
		log.Printf("Session %d set up.\n", i)
	}

	log.Printf("Sending %d messages now…\n", *numMessages)
	type benchmessage struct {
		Ts  int64
		Num int64
	}
	latencies := make(map[int64]time.Duration)
	var wg sync.WaitGroup
	wg.Add(1)
	started := time.Now()
	msgprefix := fmt.Sprintf(`PRIVMSG #bench :{"Ts":%d, "Num":`, started.UnixNano())
	go func() {
		for len(latencies) < *numMessages {
			msg, err := sessions[1].Decode()
			if err != nil {
				log.Fatal(err)
			}
			latency := time.Since(started)
			if msg.Command != irc.PRIVMSG {
				continue
			}
			var bm benchmessage
			if err := json.Unmarshal([]byte(msg.Trailing), &bm); err != nil {
				log.Fatal(err)
			}
			latencies[bm.Num] = latency
		}
		wg.Done()
	}()
	lastProgress := time.Now()
	for i := 0; i < *numMessages; i++ {
		if _, err := sessions[0].Write([]byte(msgprefix + strconv.Itoa(i) + "}\r\n")); err != nil {
			log.Fatal(err)
		}
		if time.Since(lastProgress) > 1*time.Second {
			log.Printf("[sending] %d / %d\n", i, *numMessages)
			lastProgress = time.Now()
		}
	}
	log.Printf("All messages sent.\n")
	wg.Wait()
	log.Printf("Received all messages.\n")
	for i := 0; i < *numMessages; i++ {
		log.Printf("%d: %v\n", i, latencies[int64(i)])
	}
	mps := float64(*numMessages-1) / (float64(latencies[int64(*numMessages-1)]) / float64(time.Second))
	log.Printf("%f messages/s\n", mps)

	for _, conn := range sessions {
		conn.Close()
	}

	// TODO(secure): when *numSessions > 2, log how long until the message was seen first and how long until all sessions saw it
}
