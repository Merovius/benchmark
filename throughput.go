// Continuously sends messages, trying to figure out the message/s throughput
// of the network on longer time scales. Useful to discover problems with
// snapshots/compaction.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
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
		"Number of sessions to use. The first one is used to receive messages, all others send")

	numChannels = flag.Int("channels",
		0,
		"Number of channels to use. Defaults to -sessions / 50.")

	lbPolicy = flag.String("load_balancing_policy",
		"random",
		"Load-balancing policy, determining on which sessions to send messages. Can be one of 'random' or 'round-robin'.")

	gnuplot = flag.String("gnuplot",
		"",
		"Directory in which to store GNUplot data files")
)

func main() {
	flag.Parse()

	if *numSessions < 2 {
		log.Fatal("-sessions needs to be 2 or higher (specified %d)", *numSessions)
	}

	if *numChannels == 0 {
		*numChannels = *numSessions / 50
	}

	// TODO(secure): verify that cpu governor is on performance
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	log.Printf("Joining %d channels with %d connections\n", *numChannels, *numSessions)

	sessions := make([]*irc.Conn, *numSessions)
	var sessionsMu sync.Mutex

	var wg sync.WaitGroup

	for i := 0; i < *numSessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rawconn, err := net.Dial("tcp", *target)
			if err != nil {
				log.Fatal(err)
			}
			conn := irc.NewConn(rawconn)
			sessionsMu.Lock()
			sessions[i] = conn
			sessionsMu.Unlock()
			if _, err := conn.Write([]byte(fmt.Sprintf("NICK bench-%d\r\n", i))); err != nil {
				log.Fatal(err)
			}
			if _, err := conn.Write([]byte("USER bench 0 * :bench\r\n")); err != nil {
				log.Fatal(err)
			}
			if i == 0 {
				for j := 0; j < *numChannels; j++ {
					if _, err := conn.Write([]byte(fmt.Sprintf("JOIN #bench-%d\r\n", j))); err != nil {
						log.Fatal(err)
					}
				}
			} else {
				if _, err := conn.Write([]byte(fmt.Sprintf("JOIN #bench-%d\r\n", i%*numChannels))); err != nil {
					log.Fatal(err)
				}
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

			if i > 0 {
				// On all sessions but the receiver session, discard messages.
				go func(session *irc.Conn) {
					for {
						if _, err := session.Decode(); err != nil {
							log.Fatal(err)
						}
					}
				}(conn)
			}
		}(i)
	}
	wg.Wait()
	log.Printf("All sessions set up.\n")

	var f *os.File
	if *gnuplot != "" {
		var err error
		f, err = os.Create(filepath.Join(*gnuplot, "throughput.data"))
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
	}

	numMessages := 50
	decodeChan := make(chan *irc.Message)
	go func() {
		session := sessions[0]
		for {
			msg, err := session.Decode()
			if err != nil {
				log.Fatal(err)
			}
			decodeChan <- msg
		}
	}()
	for {
		received := 0
		stop := make(chan bool)
		go func() {
			for {
				select {
				case <-stop:
					return

				case msg := <-decodeChan:
					received++

					// session[0] only receives messages, so it must ping/pong
					// with the bridge in order to not time out.
					if msg.Command == irc.PING {
						err := sessions[0].Encode(&irc.Message{
							Command:  irc.PONG,
							Params:   msg.Params,
							Trailing: msg.Trailing,
						})
						if err != nil {
							log.Fatal(err)
						}
					}
				}
			}
		}()
		started := time.Now()
		var sessidx int
		rr := 0
		for i := 0; i < numMessages; i++ {
			if *lbPolicy == "random" {
				sessidx = 1 + rand.Intn(len(sessions)-1)
			} else {
				sessidx = 1 + (rr % (len(sessions) - 1))
				rr++
			}
			msg := fmt.Sprintf(`PRIVMSG #bench-%d :{"Ts":%d, "Num":%d}`+"\r\n", sessidx%*numChannels, started.UnixNano(), i)
			if _, err := sessions[sessidx].Write([]byte(msg)); err != nil {
				log.Fatal(err)
			}
			if time.Since(started) > 1*time.Second {
				log.Printf("(aborting send at message %d, target not responsive)\n", i)
				break
			}
		}
		time.Sleep(1 * time.Second)
		stop <- true
		mps := float32(received) / float32(time.Since(started)) * float32(time.Second)
		numMessages = int(0.4*(mps*1.5) + 0.6*float32(numMessages))
		log.Printf("Received %d messages, i.e. %f mps (sending %d msgs next)\n", received, mps, numMessages)
		if *gnuplot != "" {
			fmt.Fprintf(f, "%d %f\n", time.Now().Unix(), mps)
		}
	}
}
