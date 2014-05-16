package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coreos/etcd/third_party/github.com/goraft/raft"

	etcdErr "github.com/coreos/etcd/error"
	"github.com/coreos/etcd/log"
	uhttp "github.com/coreos/etcd/pkg/http"
	"github.com/coreos/etcd/store"
)

const standbyInfoName = "standby_info"

type StandbyServerConfig struct {
	Name       string
	PeerScheme string
	PeerURL    string
	ClientURL  string
	DataDir    string
}

type standbyInfo struct {
	Cluster      []*machineMessage
	SyncInterval float64
}

type StandbyServer struct {
	Config StandbyServerConfig
	client *Client

	standbyInfo
	joinIndex uint64

	file     *os.File
	recorded bool

	removeNotify chan bool
	started      bool
	closeChan    chan bool
	routineGroup sync.WaitGroup

	sync.Mutex
}

func NewStandbyServer(config StandbyServerConfig, client *Client) (*StandbyServer, error) {
	s := &StandbyServer{
		Config:      config,
		client:      client,
		standbyInfo: standbyInfo{SyncInterval: DefaultSyncInterval},
	}
	if err := s.openStandbyInfo(); err != nil {
		return nil, fmt.Errorf("error open/create cluster info file: %v", err)
	}
	s.loadStandbyInfo()
	return s, nil
}

func (s *StandbyServer) Start() {
	s.Lock()
	defer s.Unlock()
	if s.started {
		return
	}
	s.started = true

	s.removeNotify = make(chan bool)
	s.closeChan = make(chan bool)

	s.routineGroup.Add(1)
	go func() {
		defer s.routineGroup.Done()
		s.monitorCluster()
	}()
}

// Stop stops the server gracefully.
func (s *StandbyServer) Stop() {
	s.Lock()
	defer s.Unlock()
	if !s.started {
		return
	}
	s.started = false

	close(s.closeChan)
	s.routineGroup.Wait()

	if err := s.clearStandbyInfo(); err != nil {
		log.Warnf("error clearing cluster info for standby")
	}
}

// RemoveNotify notifies the server is removed from standby mode and ready
// for peer mode. It should have joined the cluster successfully.
func (s *StandbyServer) RemoveNotify() <-chan bool {
	return s.removeNotify
}

func (s *StandbyServer) ClientHTTPHandler() http.Handler {
	return http.HandlerFunc(s.redirectRequests)
}

func (s *StandbyServer) ClusterRecorded() bool {
	return s.recorded
}

func (s *StandbyServer) ClusterURLs() []string {
	peerURLs := make([]string, 0)
	for _, peer := range s.Cluster {
		peerURLs = append(peerURLs, peer.PeerURL)
	}
	return peerURLs
}

func (s *StandbyServer) ClusterSize() int {
	return len(s.Cluster)
}

func (s *StandbyServer) setCluster(cluster []*machineMessage) {
	s.Cluster = cluster
}

func (s *StandbyServer) SyncCluster(peers []string) error {
	for i, url := range peers {
		peers[i] = s.fullPeerURL(url)
	}

	if err := s.syncCluster(peers); err != nil {
		log.Infof("fail syncing cluster(%v): %v", s.ClusterURLs(), err)
		return err
	}

	log.Infof("set cluster(%v) for standby server", s.ClusterURLs())
	return nil
}

func (s *StandbyServer) SetSyncInterval(second float64) {
	s.SyncInterval = second
}

func (s *StandbyServer) ClusterLeader() *machineMessage {
	for _, machine := range s.Cluster {
		if machine.State == raft.Leader {
			return machine
		}
	}
	return nil
}

func (s *StandbyServer) JoinIndex() uint64 {
	return s.joinIndex
}

func (s *StandbyServer) redirectRequests(w http.ResponseWriter, r *http.Request) {
	leader := s.ClusterLeader()
	if leader == nil {
		w.Header().Set("Content-Type", "application/json")
		etcdErr.NewError(etcdErr.EcodeStandbyInternal, "", 0).Write(w)
		return
	}
	uhttp.Redirect(leader.ClientURL, w, r)
}

// monitorCluster assumes that the machine has tried to join the cluster and
// failed, so it waits for the interval at the beginning.
func (s *StandbyServer) monitorCluster() {
	for {
		timer := time.NewTimer(time.Duration(int64(s.SyncInterval * float64(time.Second))))
		defer timer.Stop()
		select {
		case <-s.closeChan:
			return
		case <-timer.C:
		}

		if err := s.syncCluster(nil); err != nil {
			log.Warnf("fail syncing cluster(%v): %v", s.ClusterURLs(), err)
			continue
		}

		leader := s.ClusterLeader()
		if leader == nil {
			log.Warnf("fail getting leader from cluster(%v)", s.ClusterURLs())
			continue
		}

		if err := s.join(leader.PeerURL); err != nil {
			log.Debugf("fail joining through leader %v: %v", leader, err)
			continue
		}

		log.Infof("join through leader %v", leader.PeerURL)
		go func() {
			s.Stop()
			close(s.removeNotify)
		}()
		return
	}
}

func (s *StandbyServer) syncCluster(peerURLs []string) error {
	peerURLs = append(s.ClusterURLs(), peerURLs...)

	for _, peerURL := range peerURLs {
		// Fetch current peer list
		machines, err := s.client.GetMachines(peerURL)
		if err != nil {
			log.Debugf("fail getting machine messages from %v", peerURL)
			continue
		}

		config, err := s.client.GetClusterConfig(peerURL)
		if err != nil {
			log.Debugf("fail getting cluster config from %v", peerURL)
			continue
		}

		s.setCluster(machines)
		s.SetSyncInterval(config.SyncInterval)
		if err := s.saveStandbyInfo(); err != nil {
			log.Warnf("fail saving cluster info into disk: %v", err)
		}
		return nil
	}
	return fmt.Errorf("unreachable cluster")
}

func (s *StandbyServer) join(peer string) error {
	// Our version must match the leaders version
	version, err := s.client.GetVersion(peer)
	if err != nil {
		log.Debugf("error getting peer version")
		return err
	}
	if version < store.MinVersion() || version > store.MaxVersion() {
		log.Debugf("fail passing version compatibility(%d-%d) using %d", store.MinVersion(), store.MaxVersion(), version)
		return fmt.Errorf("incompatible version")
	}

	// Fetch cluster config to see whether exists some place.
	clusterConfig, err := s.client.GetClusterConfig(peer)
	if err != nil {
		log.Debugf("error getting cluster config")
		return err
	}
	if clusterConfig.ActiveSize <= len(s.Cluster) {
		log.Debugf("stop joining because the cluster is full with %d nodes", len(s.Cluster))
		return fmt.Errorf("out of quota")
	}

	commitIndex, err := s.client.AddMachine(peer,
		&JoinCommand{
			MinVersion: store.MinVersion(),
			MaxVersion: store.MaxVersion(),
			Name:       s.Config.Name,
			RaftURL:    s.Config.PeerURL,
			EtcdURL:    s.Config.ClientURL,
		})
	if err != nil {
		log.Debugf("error on join request")
		return err
	}
	s.joinIndex = commitIndex

	return nil
}

func (s *StandbyServer) fullPeerURL(urlStr string) string {
	u, err := url.Parse(urlStr)
	if err != nil {
		log.Warnf("fail parsing url %v", u)
		return urlStr
	}
	u.Scheme = s.Config.PeerScheme
	return u.String()
}

func (s *StandbyServer) openStandbyInfo() error {
	var err error
	path := filepath.Join(s.Config.DataDir, standbyInfoName)
	s.file, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		if os.IsNotExist(err) {
			s.file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0600)
		}
		return err
	}
	return nil
}

func (s *StandbyServer) loadStandbyInfo() ([]*machineMessage, error) {
	if _, err := s.file.Seek(0, os.SEEK_SET); err != nil {
		return nil, err
	}
	if err := json.NewDecoder(s.file).Decode(&s.standbyInfo); err != nil {
		return nil, err
	}
	s.recorded = true
	return s.standbyInfo.Cluster, nil
}

func (s *StandbyServer) saveStandbyInfo() error {
	if err := s.clearStandbyInfo(); err != nil {
		return nil
	}
	if err := json.NewEncoder(s.file).Encode(s.standbyInfo); err != nil {
		return err
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	s.recorded = true
	return nil
}

func (s *StandbyServer) clearStandbyInfo() error {
	if _, err := s.file.Seek(0, os.SEEK_SET); err != nil {
		return err
	}
	if err := s.file.Truncate(0); err != nil {
		return err
	}
	s.recorded = false
	return nil
}
