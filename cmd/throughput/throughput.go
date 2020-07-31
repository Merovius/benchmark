// Continuously sends messages, trying to figure out the message/s throughput
// of the network on longer time scales. Useful to discover problems with
// snapshots/compaction.
package main

import (
	"bytes"
	"container/ring"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	api_prometheus "github.com/prometheus/client_golang/api/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/robustirc/benchmark/internal/grafana"
	"github.com/robustirc/bridge/robustsession"
	"github.com/robustirc/robustirc/util"
	"gopkg.in/sorcix/irc.v2"
)

// token is an anonymous type used for throttling
type token struct{}

var (
	network = flag.String("network",
		"",
		`Comma-separated list of host:port addresses (e.g. "localhost:13001,localhost:13002,localhost:13003") to connect to.`)

	networkConfigFile = flag.String("network_config_file",
		"",
		"Optional filename to read the RobustIRC network config from.")

	prometheusAddr = flag.String("prometheus",
		"",
		`host:port address (e.g. "localhost:9090") of a https://prometheus.io/ instance. Required if -snapshot_dashboards is specified.`)

	snapshotDashboards = flag.String("snapshot_dashboards",
		"",
		`Comma-separated list of grafana dashboard JSON filenames to snapshot to snapshot.raintank.io. Specifying -prometheus is required when -snapshot_dashboards is specified.`)

	listen = flag.String("listen",
		"",
		"(optional) [host]:port to listen on for exporting prometheus metrics via HTTP")

	minDuration = flag.Duration("min_duration",
		1*time.Minute,
		"Minimum test runtime, regardless of whether the min/max rates have converged yet")

	numSessions = flag.Int("sessions",
		2,
		"Number of sessions to use. The first one is used to receive messages, all others send")

	numChannels = flag.Int("channels",
		0,
		"Number of channels to use. Defaults to -sessions / 50.")

	messagesReceived uint64
	messagesSent     uint64

	messageLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "message_latency",
		Help:    "Latency (in ms) between the point when the message was sent and when it was received",
		Buckets: prometheus.ExponentialBuckets(1, 2, 15),
	})

	messagesSentMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "messages_sent",
		Help: "Number of PRIVMSGs sent",
	})

	messagesReceivedMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "messages_received",
		Help: "Number of PRIVMSGs received",
	})

	spreadMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spread",
		Help: "Spread between minimum/maximum number of messages/s within the last 10s",
	})

	last10s = ring.New(10)
)

func init() {
	prometheus.MustRegister(messageLatency)
	prometheus.MustRegister(messagesSentMetric)
	prometheus.MustRegister(messagesReceivedMetric)
	prometheus.MustRegister(spreadMetric)
}

func getMin() uint64 {
	var min uint64 = ^uint64(0) // max uint64 value
	l := last10s
	for i := 0; i < last10s.Len(); i++ {
		l = l.Next()
		if l.Value == nil {
			continue
		}
		if l.Value.(uint64) < min {
			min = l.Value.(uint64)
		}
	}
	return min
}

func getMax() uint64 {
	var max uint64
	l := last10s
	for i := 0; i < last10s.Len(); i++ {
		l = l.Next()
		if l.Value == nil {
			continue
		}
		if l.Value.(uint64) > max {
			max = l.Value.(uint64)
		}
	}
	return max
}

func runWorker(tokens <-chan token, idx int) error {
	log.Printf("initializing session %d", idx)
	setupMessages := []string{
		fmt.Sprintf("NICK bench-%d\r\n", idx),
		"USER bench 0 * :bench\r\n",
	}
	if idx == 0 {
		for j := 0; j < *numChannels; j++ {
			setupMessages = append(setupMessages, fmt.Sprintf("JOIN #bench-%d\r\n", j))
		}
	} else {
		setupMessages = append(setupMessages, fmt.Sprintf("JOIN #bench-%d\r\n", idx%*numChannels))
	}
	// TODO: wait for being registered, e.g. when the nick is already in use (previous benchmark still running)?

	// Defined in github.com/robustirc/robustirc/robusthttp, which is
	// imported via github.com/robustirc/robustirc/util
	tlsCAFile := flag.Lookup("tls_ca_file").Value.String()
	session, err := robustsession.Create(*network, tlsCAFile)
	if err != nil {
		return fmt.Errorf("Could not create robustsession: %v", err)
	}
	for _, msg := range setupMessages {
		log.Printf("-> %s\n", msg)
		if err := session.PostMessage(msg); err != nil {
			return err
		}
	}

	go func() {
		for {
			// Read a token to block this goroutine until we should
			// send again. This throttles our sending speed.
			<-tokens
			if err := session.PostMessage(fmt.Sprintf("PRIVMSG #bench-%d :%d\r\n", idx%*numChannels, time.Now().UnixNano())); err != nil {
				log.Printf("PostMessage error: %v", err)
				return
			}
			atomic.AddUint64(&messagesSent, 1)
		}
	}()

	for {
		select {
		case msg := <-session.Messages:
			// Only parse each message once.
			if idx != 0 {
				continue
			}

			ircmsg := irc.ParseMessage(msg)
			if ircmsg == nil {
				continue
			}

			atomic.AddUint64(&messagesReceived, 1)

			if ircmsg.Command == irc.RPL_ENDOFNAMES {
				log.Printf("session %d set up", idx)
			}

			if ircmsg.Command != irc.PRIVMSG {
				continue
			}

			sent, err := strconv.ParseInt(ircmsg.Trailing(), 0, 64)
			if err != nil {
				continue
			}

			latency := time.Since(time.Unix(0, sent))
			latencyMs := latency.Nanoseconds() / int64(time.Millisecond)
			messageLatency.Observe(float64(latencyMs))

		case err := <-session.Errors:
			return err
		}
	}
}

func waitForHealthy(servers []string) error {
	log.Printf("Waiting for RobustIRC network to become healthy")
	started := time.Now()
	for {
		statuses, err := util.EnsureNetworkHealthy(servers, "k8s")
		if err == nil {
			return nil
		}
		log.Printf("network health: %v, statuses = %+v", err, statuses)
		if timeout := 1 * time.Minute; time.Since(started) > timeout {
			return fmt.Errorf("RobustIRC network did not become healthy within %v (error: %v)", timeout, err)
		}
		// Intentionally without exponential backoff. The RobustIRC
		// instance in question is only hit by this single throughput
		// process, so it can take 1 retry/second.
		time.Sleep(1 * time.Second)
	}
}

func waitForPrometheusHealthy(addr string) error {
	log.Printf("Waiting for Prometheus to become healthy")
	started := time.Now()
	for {
		resp, err := http.Get(fmt.Sprintf("http://%s/api/v1/query?query=count(irc_sessions)", addr))
		if err == nil {
			if got, want := resp.StatusCode, http.StatusOK; got != want {
				log.Printf("Unexpected HTTP status querying prometheus: got %d, want %d", got, want)
			} else {
				return nil
			}
		}

		if timeout := 1 * time.Minute; time.Since(started) > timeout {
			return fmt.Errorf("Prometheus did not become healthy within %v (error: %v)", timeout, err)
		}
		// Intentionally without exponential backoff. The Prometheus
		// instance in question is only hit by this single throughput
		// process, so it can take 1 retry/second.
		time.Sleep(1 * time.Second)
	}
}

func setNetworkConfigFrom(servers []string, filename string) error {
	config, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	log.Printf("Setting RobustIRC network configuration from %q", filename)
	// One try is sufficient: conflicts in network config setting can only
	// happen when something else/someone else is modifying the network config
	// at the same time. Since the RobustIRC network in question is dedicated
	// to the loadtest, we are the only modifier of configs — and if we
	// unexpectedly aren’t, failing loudly is a good thing.
	return util.SetNetworkConfig(servers, string(config), os.Getenv("ROBUSTIRC_NETWORK_PASSWORD"))
}

// snapshotMetrics queries prometheus and returns the URL of a
// snapshot of the RobustIRC grafana dashboard stored on
// snapshot.raintank.io.
func snapshotMetrics(prometheusAddr, filename string) (string, error) {
	const snapshotAPIAddr = "https://snapshot.raintank.io/api/snapshots"

	client, err := api_prometheus.New(api_prometheus.Config{Address: "http://" + prometheusAddr})
	if err != nil {
		return "", err
	}
	api := api_prometheus.NewQueryAPI(client)

	end := time.Now()
	start := end.Add(-10 * time.Minute)

	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return "", err
	}

	// Patch the dashboard definition to use the higher resolution metrics.
	// TODO: can we use variables in grafana for this?
	replaced := strings.Replace(string(data), "committed:rate5m", "committed:rate30s", -1)

	var bIn interface{}
	if err := json.Unmarshal([]byte(replaced), &bIn); err != nil {
		return "", err
	}

	bOut, err := grafana.Snapshot(bIn, api, start, end)
	if err != nil {
		return "", err
	}

	d, err := json.Marshal(&struct {
		Dashboard interface{} `json:"dashboard"`
		Expires   int         `json:"expires"`
		Name      string      `json:"name"`
	}{
		Dashboard: bOut,
		Expires:   0,
		Name:      "RobustIRC snapshot",
	})
	if err != nil {
		return "", err
	}

	resp, err := http.Post(snapshotAPIAddr, "application/json; charset=UTF-8", bytes.NewReader(d))
	if err != nil {
		return "", err
	}
	var snapshotResp struct {
		Url string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snapshotResp); err != nil {
		return "", err
	}
	return snapshotResp.Url, nil
}

func runThroughputTest() error {
	log.Printf("Joining %d channels with %d connections\n", *numChannels, *numSessions)

	tokens := make(chan token)
	for i := 0; i < *numSessions; i++ {
		go runWorker(tokens, i)
	}
	started := time.Now()
	// tokensTicker is a ticker which unblocks goroutines (“supplies a
	// token” in the typical token bucket terminology used in network
	// throttling) often enough that they can send 10000 queries/s
	// (calculated with microsecond precision).
	tokensTicker := time.Tick(time.Duration(1e6/10000) * time.Microsecond)
	every1s := time.Tick(1 * time.Second)
	every10s := time.Tick(10 * time.Second)
	currentMeasurement := last10s
	var lastSent uint64
	var lastReceived uint64
	for {
		select {
		case <-every1s:
			sent := atomic.LoadUint64(&messagesSent)
			received := atomic.LoadUint64(&messagesReceived)
			currentMeasurement = currentMeasurement.Next()
			currentMeasurement.Value = received - lastReceived
			min := getMin()
			max := getMax()
			spread := max - min
			log.Printf("sent %d, recv %d, (last 10s) min = %d, max = %d, spread = %d", sent-lastSent, received-lastReceived, min, max, spread)

			messagesSentMetric.Add(float64(sent - lastSent))
			messagesReceivedMetric.Add(float64(received - lastReceived))
			spreadMetric.Set(float64(spread))

			lastSent = sent
			lastReceived = received

		case <-every10s:
			if time.Since(started) <= *minDuration {
				continue
			}
			min := getMin()
			max := getMax()
			spread := max - min

			if float32(spread)/float32(max) < 0.1 {
				log.Printf("converged! spread is < 10%%")
				return nil
			}

			// This approach does not work well: it just permanently reduces the qps, without any clear improvement in spread
			// targetQps := (1 - (spread / max)) * max
			// log.Printf("re-adjusting: targetQps = %v", targetQps)
			// tokensTicker = time.Tick(time.Duration(1e6/targetQps) * time.Microsecond)

		case <-tokensTicker:
			tokens <- token{}
		}
	}
}

const (
	// Specified in rlt/static/prometheus/prometheus.conf
	prometheusScrapeInterval = 15 * time.Second
)

func main() {
	flag.Parse()

	if *numSessions < 2 {
		log.Fatal("-sessions needs to be 2 or higher (specified %d)", *numSessions)
	}

	if *numChannels == 0 {
		*numChannels = *numSessions / 50
	}
	if *numChannels < 1 {
		*numChannels = 1
	}

	// Wait for the network and prometheus to become healthy.
	servers := strings.Split(*network, ",")
	if err := waitForHealthy(servers); err != nil {
		log.Fatal(err)
	}

	if *networkConfigFile != "" {
		if err := setNetworkConfigFrom(servers, *networkConfigFile); err != nil {
			log.Fatal(err)
		}
	}

	if *prometheusAddr != "" {
		if err := waitForPrometheusHealthy(*prometheusAddr); err != nil {
			log.Fatal(err)
		}
	}

	if *listen != "" {
		go func() {
			log.Printf("Listening on %q", *listen)
			http.Handle("/metrics", prometheus.Handler())
			log.Fatal(http.ListenAndServe(*listen, nil))
		}()
	}

	// TODO(secure): verify that cpu governor is on performance
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	if err := runThroughputTest(); err != nil {
		log.Fatal(err)
	}

	if *snapshotDashboards != "" && *prometheusAddr != "" {
		log.Printf("Giving Prometheus another %v (scrape_interval)", prometheusScrapeInterval)
		time.Sleep(prometheusScrapeInterval)

		for _, filename := range strings.Split(*snapshotDashboards, ",") {
			snapshotUrl, err := snapshotMetrics(*prometheusAddr, filename)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("RobustIRC dashboard snapshot stored at %s", snapshotUrl)
		}
	}
}
