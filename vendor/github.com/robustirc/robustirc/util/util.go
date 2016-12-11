package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/golang/protobuf/proto"
	"github.com/robustirc/robustirc/robusthttp"
	"github.com/robustirc/robustirc/types"
	"github.com/sorcix/irc"

	pb "github.com/robustirc/robustirc/proto"
)

type ServerStatus struct {
	Server string

	State          string
	Leader         string
	Peers          []string
	AppliedIndex   uint64
	CommitIndex    uint64
	LastContact    time.Time
	ExecutableHash string
	CurrentTime    time.Time
}

func GetServerStatus(server, networkPassword string) (ServerStatus, error) {
	var status ServerStatus
	if !strings.HasPrefix(server, "https://") {
		server = fmt.Sprintf("https://%s/", server)
	}
	req, err := http.NewRequest("GET", server, nil)
	if err != nil {
		return status, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := robusthttp.Client(networkPassword, true).Do(req)
	if err != nil {
		return status, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return status, fmt.Errorf("Expected HTTP OK, got %v", resp.Status)
	}

	err = json.NewDecoder(resp.Body).Decode(&status)
	ioutil.ReadAll(resp.Body)
	return status, err
}

func CollectStatuses(servers []string, networkPassword string) (map[string]ServerStatus, error) {
	var (
		statuses   = make(map[string]ServerStatus, len(servers))
		statusesMu sync.Mutex
		g          errgroup.Group
	)
	setStatus := func(status ServerStatus) {
		statusesMu.Lock()
		defer statusesMu.Unlock()
		statuses[status.Server] = status
	}
	for _, server := range servers {
		server := server // capture range variable
		g.Go(func() error {
			status, err := GetServerStatus(server, networkPassword)
			if err != nil {
				return err
			}
			status.Server = server
			setStatus(status)
			return nil
		})
	}
	return statuses, g.Wait()
}

// EnsureNetworkHealthy returns nil when all of the following is true:
//  • all nodes are reachable
//  • all nodes return the same leader
//  • all nodes are either follower or leader (i.e. not candidate/initializing)
//  • all follower nodes were recently contacted by the leader
func EnsureNetworkHealthy(servers []string, networkPassword string) (map[string]ServerStatus, error) {
	var leader string

	statuses, err := CollectStatuses(servers, networkPassword)
	if err != nil {
		return statuses, err
	}

	for _, status := range statuses {
		if status.State != "Leader" && status.State != "Follower" {
			return statuses, fmt.Errorf("Server %q in state %q, need Leader or Follower",
				status.Server, status.State)
		}
		if leader == "" {
			leader = status.Leader
		} else if leader != status.Leader {
			return statuses, fmt.Errorf("Server %q thinks %q is leader, others think %q is leader",
				status.Server, status.Leader, leader)
		}
		if status.State == "Follower" && time.Since(status.LastContact) > 2*time.Second {
			return statuses, fmt.Errorf("Server %q was last contacted by the leader at %v, which is over 2 seconds ago",
				status.Server, status.LastContact)
		}
	}
	if leader == "" {
		return statuses, fmt.Errorf("There is no leader currently")
	}
	return statuses, nil
}

func ResolveNetwork(network string) []string {
	var servers []string

	parts := strings.Split(network, ",")
	if len(parts) > 1 {
		log.Printf("Interpreting %q as list of servers instead of network name\n", network)
		for _, part := range parts {
			if strings.TrimSpace(part) != "" {
				servers = append(servers, part)
			}
		}
		return servers
	}

	_, addrs, err := net.LookupSRV("robustirc", "tcp", network)
	if err != nil {
		log.Fatal(err)
	}
	for _, addr := range addrs {
		target := addr.Target
		if target[len(target)-1] == '.' {
			target = target[:len(target)-1]
		}
		servers = append(servers, fmt.Sprintf("%s:%d", target, addr.Port))
	}

	return servers
}

func SetNetworkConfig(servers []string, config, networkPassword string) error {
	server := servers[0]
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/config", server), nil)
	if err != nil {
		return err
	}
	resp, err := robusthttp.Client(networkPassword, true).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Expected HTTP OK, got %v", resp.Status)
	}

	req, err = http.NewRequest("POST", fmt.Sprintf("https://%s/config", server), bytes.NewBuffer([]byte(config)))
	if err != nil {
		return err
	}
	req.Header.Set("X-RobustIRC-Config-Revision", resp.Header.Get("X-RobustIRC-Config-Revision"))
	resp, err = robusthttp.Client(networkPassword, true).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Expected HTTP OK, got %v", resp.Status)
	}

	return nil
}

func PrivacyFilterSnapshot(snapshot pb.Snapshot) pb.Snapshot {
	result := proto.Clone(&snapshot).(*pb.Snapshot)
	for _, session := range result.Sessions {
		session.Pass = "<privacy filtered>"
	}
	return *result
}

func PrivacyFilterIrcmsg(message *irc.Message) *irc.Message {
	if message == nil {
		return nil
	}
	if message.Command == irc.PRIVMSG || message.Command == irc.NOTICE {
		message.Trailing = "<privacy filtered>"
	}
	if message.Command == irc.PASS {
		message.Params = []string{"<privacy filtered>"}
		message.Trailing = ""
	}
	return message
}

func PrivacyFilterMsg(message *types.RobustMessage) *types.RobustMessage {
	return &types.RobustMessage{
		Id:      message.Id,
		Session: message.Session,
		Type:    message.Type,
		Data:    PrivacyFilterIrcmsg(irc.ParseMessage(message.Data)).String(),
	}
}

func PrivacyFilterMsgs(messages []*types.RobustMessage) []*types.RobustMessage {
	output := make([]*types.RobustMessage, len(messages))
	for idx, message := range messages {
		output[idx] = PrivacyFilterMsg(message)
	}
	return output
}
