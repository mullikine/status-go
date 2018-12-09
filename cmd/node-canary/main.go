// node-canary tests whether a P2P peer is responding correctly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	stdlog "log"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto/sha3"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/status-im/status-go/api"
	"github.com/status-im/status-go/logutils"
	"github.com/status-im/status-go/params"
	"github.com/status-im/status-go/rpc"
	"github.com/status-im/status-go/services/shhext"
	"github.com/status-im/status-go/t/helpers"
	whisper "github.com/status-im/whisper/whisperv6"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	mailboxPassword = "status-offline-inbox"
)

// All general log messages in this package should be routed through this logger.
var logger = log.New("package", "status-go/cmd/node-canary")

var (
	staticEnodeAddr     = flag.String("staticnode", "", "static node enode address to test (e.g. enode://3f04db09bedc8d85a198de94c84da73aa7782fafc61b28c525ec5cca5a6cc16be7ebbb5cd001780f71d8408d35a2f6326faa1e524d9d8875294172ebec988743@172.16.238.10:30303)")
	mailserverEnodeAddr = flag.String("mailserver", "", "mailserver enode address to test (e.g. enode://1da276e34126e93babf24ec88aac1a7602b4cbb2e11b0961d0ab5e989ca9c261aa7f7c1c85f15550a5f1e5a5ca2305b53b9280cf5894d5ecf7d257b173136d40@167.99.209.61:30504)")
	publicChannel       = flag.String("channel", "status", "The public channel name to retrieve historic messages from (used with 'mailserver' flag)")
	timeout             = flag.Int("timeout", 10, "Timeout when connecting to node or fetching messages from mailserver, in seconds")
	period              = flag.Int("period", 24*60*60, "How far in the past to request messages from mailserver, in seconds")
	minPow              = flag.Float64("shh.pow", params.WhisperMinimumPoW, "PoW for messages to be added to queue, in float format")
	ttl                 = flag.Int("shh.ttl", params.WhisperTTL, "Time to live for messages, in seconds")
	homePath            = flag.String("home-dir", ".", "Home directory where state is stored")
	logLevel            = flag.String("log", "INFO", `Log level, one of: "ERROR", "WARN", "INFO", "DEBUG", and "TRACE"`)
	logFile             = flag.String("logfile", "", "Path to the log file")
	logWithoutColors    = flag.Bool("log-without-color", false, "Disables log colors")
)

func main() {
	var err error
	var staticParsedNode, mailserverParsedNode *enode.Node
	if *staticEnodeAddr != "" {
		staticParsedNode, err = enode.ParseV4(*staticEnodeAddr)
		if err != nil {
			logger.Crit("Invalid static address specified", "staticEnodeAddr", *staticEnodeAddr, "error", err)
			os.Exit(1)
		}
	}

	if *mailserverEnodeAddr != "" {
		mailserverParsedNode, err = enode.ParseV4(*mailserverEnodeAddr)
		if err != nil {
			logger.Crit("Invalid mailserver address specified", "mailserverEnodeAddr", *mailserverEnodeAddr, "error", err)
			os.Exit(1)
		}
	}

	if staticParsedNode != nil {
		verifyStaticNodeBehavior(staticParsedNode)
		logger.Info("Connected to static node correctly", "address", *staticEnodeAddr)
		os.Exit(0)
	}

	if mailserverParsedNode != nil {
		verifyMailserverBehavior(mailserverParsedNode)
		logger.Info("Mailserver responded correctly", "address", *mailserverEnodeAddr)
		os.Exit(0)
	}

	logger.Crit("No address specified")
	os.Exit(1)
}

func init() {
	flag.Parse()

	colors := !(*logWithoutColors)
	if colors {
		colors = terminal.IsTerminal(int(os.Stdin.Fd()))
	}

	if err := logutils.OverrideRootLog(*logLevel != "", *logLevel, *logFile, colors); err != nil {
		stdlog.Fatalf("Error initializing logger: %s", err)
	}
}

func verifyMailserverBehavior(mailserverNode *enode.Node) {
	clientBackend, err := startClientNode()
	if err != nil {
		logger.Error("Node start failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = clientBackend.StopNode() }()

	clientNode := clientBackend.StatusNode()
	clientWhisperService, err := clientNode.WhisperService()
	if err != nil {
		logger.Error("Could not retrieve Whisper service", "error", err)
		os.Exit(1)
	}
	clientShhExtService, err := clientNode.ShhExtService()
	if err != nil {
		logger.Error("Could not retrieve shhext service", "error", err)
		os.Exit(1)
	}

	// add mailserver peer to client
	clientErrCh := helpers.WaitForPeerAsync(clientNode.Server(), *mailserverEnodeAddr, p2p.PeerEventTypeAdd, time.Duration(*timeout)*time.Second)

	err = clientNode.AddPeer(*mailserverEnodeAddr)
	if err != nil {
		logger.Error("Failed to add mailserver peer to client", "error", err)
		os.Exit(1)
	}

	err = <-clientErrCh
	if err != nil {
		logger.Error("Error detected while waiting for mailserver peer to be added", "error", err)
		os.Exit(1)
	}

	// add mailserver sym key
	mailServerKeyID, err := clientWhisperService.AddSymKeyFromPassword(mailboxPassword)
	if err != nil {
		logger.Error("Error adding mailserver sym key to client peer", "error", err)
		os.Exit(1)
	}

	mailboxPeer := mailserverNode.ID().Bytes()
	err = clientWhisperService.AllowP2PMessagesFromPeer(mailboxPeer)
	if err != nil {
		logger.Error("Failed to allow P2P messages from mailserver peer", "error", err, mailserverNode.String())
		os.Exit(1)
	}

	clientRPCClient := clientNode.RPCClient()

	// TODO: Replace chat implementation with github.com/status-im/status-go-sdk
	_, topic, _, err := joinPublicChat(clientWhisperService, clientRPCClient, *publicChannel)
	if err != nil {
		logger.Error("Failed to join public chat", "error", err)
		os.Exit(1)
	}

	// watch for envelopes to be available in filters in the client
	envelopeAvailableWatcher := make(chan whisper.EnvelopeEvent, 1024)
	sub := clientWhisperService.SubscribeEnvelopeEvents(envelopeAvailableWatcher)
	defer sub.Unsubscribe()

	// watch for mailserver responses in the client
	mailServerResponseWatcher := make(chan whisper.EnvelopeEvent, 1024)
	sub = clientWhisperService.SubscribeEnvelopeEvents(mailServerResponseWatcher)
	defer sub.Unsubscribe()

	// request messages from mailbox
	shhextAPI := shhext.NewPublicAPI(clientShhExtService)
	requestIDBytes, err := shhextAPI.RequestMessages(context.TODO(),
		shhext.MessagesRequest{
			MailServerPeer: mailserverNode.String(),
			From:           uint32(clientWhisperService.GetCurrentTime().Add(-time.Duration(*period) * time.Second).Unix()),
			Limit:          1,
			Topic:          topic,
			SymKeyID:       mailServerKeyID,
			Timeout:        time.Duration(*timeout),
		})
	if err != nil {
		logger.Error("Error requesting historic messages from mailserver", "error", err)
		os.Exit(2)
	}
	requestID := common.BytesToHash(requestIDBytes)

	// wait for mailserver request sent event
	err = waitForMailServerRequestSent(mailServerResponseWatcher, requestID, time.Duration(*timeout)*time.Second)
	if err != nil {
		logger.Error("Error waiting for mailserver request sent event", "error", err)
		os.Exit(3)
	}

	// wait for mailserver response
	resp, err := waitForMailServerResponse(mailServerResponseWatcher, requestID, time.Duration(*timeout)*time.Second)
	if err != nil {
		logger.Error("Error waiting for mailserver response", "error", err)
		os.Exit(3)
	}

	// wait for last envelope sent by the mailserver to be available for filters
	err = waitForEnvelopeEvents(envelopeAvailableWatcher, []string{resp.LastEnvelopeHash.String()}, whisper.EventEnvelopeAvailable)
	if err != nil {
		logger.Error("Error waiting for envelopes to be available to client filter", "error", err)
		os.Exit(4)
	}
}

func verifyStaticNodeBehavior(staticNode *enode.Node) {
	clientBackend, err := startClientNode()
	if err != nil {
		logger.Error("Node start failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = clientBackend.StopNode() }()

	clientNode := clientBackend.StatusNode()

	// wait for peer to be added to client
	clientErrCh := helpers.WaitForPeerAsync(clientNode.Server(), *staticEnodeAddr, p2p.PeerEventTypeAdd, 5*time.Second)
	err = <-clientErrCh
	if err != nil {
		logger.Error("Error detected while waiting for static peer to be added", "error", err)
		os.Exit(1)
	}

	// wait to check if peer remains connected to client
	clientErrCh = helpers.WaitForPeerAsync(clientNode.Server(), *staticEnodeAddr, p2p.PeerEventTypeDrop, 5*time.Second)
	err = <-clientErrCh
	peers := clientNode.GethNode().Server().Peers()
	if len(peers) != 1 {
		logger.Error("Failed to add static peer", "error", err)
		os.Exit(1)
	}
}

// makeNodeConfig parses incoming CLI options and returns node configuration object
func makeNodeConfig() (*params.NodeConfig, error) {
	err := error(nil)

	workDir := ""
	if path.IsAbs(*homePath) {
		workDir = *homePath
	} else {
		workDir, err = filepath.Abs(filepath.Dir(os.Args[0]))
		if err == nil {
			workDir = path.Join(workDir, *homePath)
		}
	}
	if err != nil {
		return nil, err
	}

	nodeConfig, err := params.NewNodeConfigWithDefaults(path.Join(workDir, ".ethereum"), uint64(params.RopstenNetworkID))
	if err != nil {
		return nil, err
	}

	if *logLevel != "" {
		nodeConfig.LogLevel = *logLevel
		nodeConfig.LogEnabled = true
	}

	if *logFile != "" {
		nodeConfig.LogFile = *logFile
	}

	nodeConfig.NoDiscovery = true
	nodeConfig.ListenAddr = ""
	if *staticEnodeAddr != "" {
		nodeConfig.ClusterConfig.Enabled = true
		nodeConfig.ClusterConfig.Fleet = params.FleetUndefined
		nodeConfig.ClusterConfig.StaticNodes = []string{
			*staticEnodeAddr,
		}
	}

	return whisperConfig(nodeConfig)
}

// whisperConfig creates node configuration object from flags
func whisperConfig(nodeConfig *params.NodeConfig) (*params.NodeConfig, error) {
	whisperConfig := &nodeConfig.WhisperConfig
	whisperConfig.Enabled = true
	whisperConfig.LightClient = true
	whisperConfig.MinimumPoW = *minPow
	whisperConfig.TTL = *ttl
	whisperConfig.EnableNTPSync = true

	return nodeConfig, nil
}

func startClientNode() (*api.StatusBackend, error) {
	config, err := makeNodeConfig()
	if err != nil {
		return nil, err
	}
	clientBackend := api.NewStatusBackend()
	err = clientBackend.StartNode(config)
	if err != nil {
		return nil, err
	}
	return clientBackend, err
}

func joinPublicChat(w *whisper.Whisper, rpcClient *rpc.Client, name string) (string, whisper.TopicType, string, error) {
	keyID, err := w.AddSymKeyFromPassword(name)
	if err != nil {
		return "", whisper.TopicType{}, "", err
	}

	h := sha3.NewKeccak256()
	_, err = h.Write([]byte(name))
	if err != nil {
		return "", whisper.TopicType{}, "", err
	}
	fullTopic := h.Sum(nil)
	topic := whisper.BytesToTopic(fullTopic)

	whisperAPI := whisper.NewPublicWhisperAPI(w)
	filterID, err := whisperAPI.NewMessageFilter(whisper.Criteria{SymKeyID: keyID, Topics: []whisper.TopicType{topic}})

	return keyID, topic, filterID, err
}

func waitForMailServerRequestSent(events chan whisper.EnvelopeEvent, requestID common.Hash, timeout time.Duration) error {
	timeoutTimer := time.NewTimer(timeout)
	for {
		select {
		case event := <-events:
			if event.Hash == requestID && event.Event == whisper.EventMailServerRequestSent {
				timeoutTimer.Stop()
				return nil
			}
		case <-timeoutTimer.C:
			return errors.New("timed out waiting for mailserver request sent")
		}
	}
}

func waitForMailServerResponse(events chan whisper.EnvelopeEvent, requestID common.Hash, timeout time.Duration) (*whisper.MailServerResponse, error) {
	timeoutTimer := time.NewTimer(timeout)
	for {
		select {
		case event := <-events:
			if event.Hash == requestID {
				resp, err := decodeMailServerResponse(event)
				if resp != nil || err != nil {
					timeoutTimer.Stop()
					return resp, err
				}
			}
		case <-timeoutTimer.C:
			return nil, errors.New("timed out waiting for mailserver response")
		}
	}
}

func decodeMailServerResponse(event whisper.EnvelopeEvent) (*whisper.MailServerResponse, error) {
	switch event.Event {
	case whisper.EventMailServerRequestSent:
		return nil, nil
	case whisper.EventMailServerRequestCompleted:
		resp, ok := event.Data.(*whisper.MailServerResponse)
		if !ok {
			return nil, errors.New("failed to convert event to a *MailServerResponse")
		}

		return resp, nil
	case whisper.EventMailServerRequestExpired:
		return nil, errors.New("no messages available from mailserver")
	default:
		return nil, fmt.Errorf("unexpected event type: %v", event.Event)
	}
}

func waitForEnvelopeEvents(events chan whisper.EnvelopeEvent, hashes []string, event whisper.EventType) error {
	check := make(map[string]struct{})
	for _, hash := range hashes {
		check[hash] = struct{}{}
	}

	timeout := time.NewTimer(time.Second * 5)
	for {
		select {
		case e := <-events:
			if e.Event == event {
				delete(check, e.Hash.String())
				if len(check) == 0 {
					timeout.Stop()
					return nil
				}
			}
		case <-timeout.C:
			return fmt.Errorf("timed out while waiting for event on envelopes. event: %s", event)
		}
	}
}
