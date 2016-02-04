// The stats service collects the exported stats and submits them to
// the Kapacitor stream under the configured database and retetion policy.
//
// If you want to persist the data to InfluxDB just add a task like so:
//
// Example:
//    stream
//        .from()
//        .influxDBOut()
//
// Assuming using default database and retetion policy run:
// `kapacitor define -name _stats -type stream -tick path/to/above/script.tick -dbrp _kapacitor.default`
//
// If you do create a task to send the data to InfluxDB make sure not to subscribe to that data in InfluxDB.
//
// Example:
//
// [influxdb]
//     ...
//     [influxdb.excluded-subscriptions]
//         _kapacitor = [ "default" ]
//
package stats

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/influxdata/kapacitor"
	"github.com/influxdata/kapacitor/models"
)

// Sends internal stats back into the Kapacitor stream.
// Internal stats come from running tasks and other
// services running within Kapacitor.
type Service struct {
	TaskMaster interface {
		Stream(name string) (kapacitor.StreamCollector, error)
	}

	stream kapacitor.StreamCollector

	interval time.Duration
	db       string
	rp       string

	open    bool
	closing chan struct{}
	mu      sync.Mutex
	wg      sync.WaitGroup

	logger *log.Logger

	enterpriseHosts []*client.Host

	clusterID string
	productID string
	hostname  string
	version   string
	product   string
}

func NewService(c Config, l *log.Logger) *Service {
	return &Service{
		interval:        time.Duration(c.StatsInterval),
		db:              c.Database,
		rp:              c.RetentionPolicy,
		logger:          l,
		enterpriseHosts: c.EnterpriseHosts,
	}
}

func (s *Service) Open() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Populate published vars
	s.clusterID = kapacitor.GetStringVar(kapacitor.ClusterIDVarName)
	s.productID = kapacitor.GetStringVar(kapacitor.ServerIDVarName)
	s.hostname = kapacitor.GetStringVar(kapacitor.HostVarName)
	s.version = kapacitor.GetStringVar(kapacitor.VersionVarName)
	s.product = kapacitor.Product

	s.stream, err = s.TaskMaster.Stream("stats")
	if err != nil {
		return
	}
	s.open = true
	s.closing = make(chan struct{})

	if err := s.registerServer(); err != nil {
		s.logger.Println("E! Unable to register with Enterprise Manager")
	}
	s.wg.Add(1)
	go s.sendStats()
	s.logger.Println("I! opened service")
	return
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return errors.New("error closing stats service: service not open")
	}
	s.open = false
	close(s.closing)
	s.wg.Wait()
	s.stream.Close()
	s.logger.Println("I! closed service")
	return nil
}

func (s *Service) registerServer() error {
	if !s.enabled || len(s.enterpriseHosts) == 0 {
		return nil
	}

	cl, err := client.New(s.enterpriseHosts)
	if err != nil {
		s.logger.Printf("E! Unable to contact one or more Enterprise hosts: %s\n", err.Error())
		return err
	}

	product := client.Product{
		ClusterID: s.clusterID,
		ProductID: s.productID,
		Host:      s.hostname,
		Name:      s.product,
		Version:   s.version,
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		resp, err := cl.Register(&product)

		if err != nil {
			s.logger.Printf("failed to register Kapacitor with %s, received code %s, error: %s", resp.Response.Request.URL.String(), resp.Status, err)
			return
		}
	}()
	return nil
}

func (s *Service) sendStats() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.closing:
			return
		case <-ticker.C:
			s.reportStats()
		}
	}
}

func (s *Service) reportStats() {
	now := time.Now().UTC()
	data, err := kapacitor.GetStatsData()
	if err != nil {
		s.logger.Println("E! error getting stats data:", err)
		return
	}
	for _, stat := range data {
		p := models.Point{
			Database:        s.db,
			RetentionPolicy: s.rp,
			Name:            stat.Name,
			Group:           models.NilGroup,
			Tags:            models.Tags(stat.Tags),
			Time:            now,
			Fields:          models.Fields(stat.Values),
		}
		s.stream.CollectPoint(p)
	}
}
