package meta

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/influxdata/influxdb"
	"github.com/zhexuany/influxdb-cluster/meta/internal"

	"github.com/gogo/protobuf/proto"
	"github.com/hashicorp/raft"
)

// Retention policy settings.
const (
	autoCreateRetentionPolicyName   = "default"
	autoCreateRetentionPolicyPeriod = 0

	// maxAutoCreatedRetentionPolicyReplicaN is the maximum replication factor that will
	// be set for auto-created retention policies.
	maxAutoCreatedRetentionPolicyReplicaN = 3
)

// Raft configuration.
const (
	raftListenerStartupTimeout = time.Second
)

type store struct {
	mu      sync.RWMutex
	closing chan struct{}

	config      *Config
	data        *Data
	raftState   *raftState
	dataChanged chan struct{}
	path        string
	opened      bool
	logger      *log.Logger

	raftAddr string
	httpAddr string

	node *influxdb.Node
}

// newStore will create a new metastore with the passed in config
func newStore(c *Config, httpAddr, raftAddr string) *store {
	s := store{
		data: &Data{
			Index: 1,
		},
		closing:     make(chan struct{}),
		dataChanged: make(chan struct{}),
		path:        c.Dir,
		config:      c,
		httpAddr:    httpAddr,
		raftAddr:    raftAddr,
	}
	if c.LoggingEnabled {
		s.logger = log.New(os.Stderr, "[metastore] ", log.LstdFlags)
	} else {
		s.logger = log.New(ioutil.Discard, "", 0)
	}

	return &s
}

// open opens and initializes the raft store.
func (s *store) open(raftln net.Listener) error {
	s.logger.Printf("Using data dir: %v", s.path)

	joinPeers, err := s.filterAddr(s.config.JoinPeers, s.httpAddr)
	if err != nil {
		return err
	}
	joinPeers = s.config.JoinPeers

	var initializePeers []string
	if len(joinPeers) > 0 {
		c := NewClient()
		c.SetMetaServers(joinPeers)
		c.SetTLS(s.config.HTTPSEnabled)
		for {
			peers := c.peers()
			if !Peers(peers).Contains(s.raftAddr) {
				peers = append(peers, s.raftAddr)
			}
			if len(s.config.JoinPeers)-len(peers) == 0 {
				initializePeers = peers
				break
			}

			if len(peers) > len(s.config.JoinPeers) {
				s.logger.Printf("waiting for join peers to match config specified. found %v, config specified %v", peers, s.config.JoinPeers)
			} else {
				s.logger.Printf("Waiting for %d join peers.  Have %v. Asking nodes: %v", len(s.config.JoinPeers)-len(peers), peers, joinPeers)
			}
			time.Sleep(time.Second)
		}
	}

	if err := s.setOpen(); err != nil {
		return err
	}

	// Create the root directory if it doesn't already exist.
	if err := os.MkdirAll(s.path, 0777); err != nil {
		return fmt.Errorf("mkdir all: %s", err)
	}

	// Open the raft store.
	if err := s.openRaft(initializePeers, raftln); err != nil {
		return fmt.Errorf("raft: %s", err)
	}

	if len(joinPeers) > 0 {
		c := NewClient()
		c.SetMetaServers(joinPeers)
		c.SetTLS(s.config.HTTPSEnabled)
		if err := c.Open(); err != nil {
			return err
		}
		defer c.Close()

		n, err := c.JoinMetaServer(s.httpAddr, s.raftAddr)
		if err != nil {
			return err
		}
		s.node.ID = n.ID
		if err := s.node.Save(); err != nil {
			return err
		}

	}

	// Wait for a leader to be elected so we know the raft log is loaded
	// and up to date
	if err := s.waitForLeader(0); err != nil {
		return err
	}

	// Make sure this server is in the list of metanodes
	peers, err := s.raftState.peers()
	if err != nil {
		return err
	}
	if len(peers) <= 1 {
		// we have to loop here because if the hostname has changed
		// raft will take a little bit to normalize so that this host
		// will be marked as the leader
		for {
			err := s.setMetaNode(s.httpAddr, s.raftAddr)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	return nil
}

func (s *store) setOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Check if store has already been opened.
	if s.opened {
		return ErrStoreOpen
	}
	s.opened = true
	return nil
}

// peers returns the raft peers known to this store
func (s *store) peers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil {
		return []string{s.raftAddr}
	}
	peers, err := s.raftState.peers()
	if err != nil {
		return []string{s.raftAddr}
	}
	return peers
}

func (s *store) openRaft(initializePeers []string, raftln net.Listener) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := newRaftState(s.config, s.raftAddr)
	rs.logger = s.logger
	rs.path = s.path

	if err := rs.open(s, raftln, initializePeers); err != nil {
		return err
	}
	s.raftState = rs

	return nil
}

func (s *store) raftOpened() bool {
	return s.opened
}

func (s *store) ready() bool {
}

func (s *store) reset() error {

}

func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.closing:
		// already closed
		return nil
	default:
		close(s.closing)
		return s.raftState.close()
	}
}

func (s *store) snapshot() (*Data, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Clone(), nil
}

//TODO
func (s *store) setSnapshot(data *Data) {}

// afterIndex returns a channel that will be closed to signal
// the caller when an updated snapshot is available.
func (s *store) afterIndex(index uint64) <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if index < s.data.Index {
		// Client needs update so return a closed channel.
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	return s.dataChanged
}

//TODO
func (s *store) applied() {}

// WaitForLeader sleeps until a leader is found or a timeout occurs.
// timeout == 0 means to wait forever.
func (s *store) waitForLeader(timeout time.Duration) error {
	// Begin timeout timer.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Continually check for leader until timeout.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.closing:
			return errors.New("closing")
		case <-timer.C:
			if timeout != 0 {
				return errors.New("timeout")
			}
		case <-ticker.C:
			if s.leader() != "" {
				return nil
			}
		}
	}
}

// isLeader returns true if the store is currently the leader.
func (s *store) isLeader() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil {
		return false
	}
	return s.raftState.raft.State() == raft.Leader
}

// leader returns what the store thinks is the current leader. An empty
// string indicates no leader exists.
func (s *store) leader() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil || s.raftState.raft == nil {
		return ""
	}
	return s.raftState.raft.Leader()
}

// leaderHTTP returns the HTTP API connection info for the metanode
// that is the raft leader
func (s *store) leaderHTTP() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil {
		return ""
	}
	l := s.raftState.raft.Leader()

	for _, n := range s.data.MetaNodes {
		if n.TCPHost == l {
			return n.Host
		}
	}

	return ""
}

// otherMetaServersHTTP will return the HTTP bind addresses of the other
// meta servers in the cluster
func (s *store) otherMetaServersHTTP() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var a []string
	for _, n := range s.data.MetaNodes {
		if n.TCPHost != s.raftAddr {
			a = append(a, n.Host)
		}
	}
	return a
}

func (s *store) dataNode() meta.NodeInfos {
	return s.data.DataNodes
}

func (s *store) dataNodeByTCPHost() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var a []string
	for _, n := range s.data.DataNodes {
		a = append(a, n.Host)
	}
	return a
}

// index returns the current store index.
func (s *store) index() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Index
}

// apply applies a command to raft.
func (s *store) apply(b []byte) error {
	if s.raftState == nil {
		return fmt.Errorf("store not open")
	}
	return s.raftState.apply(b)
}

// join adds a new server to the metaservice and raft
func (s *store) join(n *NodeInfo) (*NodeInfo, error) {
	s.mu.RLock()
	if s.raftState == nil {
		s.mu.RUnlock()
		return nil, fmt.Errorf("store not open")
	}
	if err := s.raftState.addPeer(n.TCPHost); err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	s.mu.RUnlock()

	if err := s.createMetaNode(n.Host, n.TCPHost); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, node := range s.data.MetaNodes {
		if node.TCPHost == n.TCPHost && node.Host == n.Host {
			return &node, nil
		}
	}
	return nil, ErrNodeNotFound
}

// leave removes a server from the metaservice and raft
func (s *store) leave(n *NodeInfo) error {
	return s.raftState.removePeer(n.TCPHost)
}

func (s *store) removePeer(peer string) error {
	return s.raftState.removePeer(peer)
}

// createMetaNode is used by the join command to create the metanode int
// the metastore
func (s *store) createMetaNode(addr, raftAddr string) error {
	val := &internal.CreateMetaNodeCommand{
		HTTPAddr: proto.String(addr),
		TCPAddr:  proto.String(raftAddr),
		Rand:     proto.Uint64(uint64(rand.Int63())),
	}
	t := internal.Command_CreateMetaNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_CreateMetaNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}

func (s *store) deleteMetaNode(addr, raftAddr string) error {
}

func (s *store) createDataNode() error {
}
func (s *store) deleteDataNode() error {
}
func (s *store) updateDataNode() error {
}
func (s *store) nodeByHTTPAddr() error {
}
func (s *store) copyShard() error {
}
func (s *store) removeShard() error {
}
func (s *store) killCopyShard() error {
}

// remoteNodeError.Error() {

func (s *store) executeCopyShardStatus() error {
}
func (s *store) copyShardStatus() error {
}
func (s *store) user() error {
}
func (s *store) users() error {
}
func (s *store) adminExists() error {
}
func (s *store) createUser() error {
}
func (s *store) deleteUser() error {
}
func (s *store) setUserPassword() error {
}
func (s *store) addUserPermissions() error {
}
func (s *store) removeUserPermissions() error {
}
func (s *store) role() error {
}
func (s *store) roles() error {
}
func (s *store) createRole() error {
}
func (s *store) deleteRole() {
}
func (s *store) addRoleUsers() error {
}
func (s *store) removeRoleUsers() error {
}
func (s *store) addRolePermissions() erorr {
}
func (s *store) removeRolePermissions() error {
}
func (s *store) changeRoleName() error {
}
func (s *store) truncateShardGroups() error {
}
func (s *store) authenticate() error {
}
func (s *store) authorized() error {
}
func (s *store) authorizedScoped() error {
}
func (s *store) updateRetentionPolicy() error {
}
