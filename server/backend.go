package server

import (
	"github.com/VividCortex/ewma"
	"github.com/klauspost/shutdown"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// A backend is a single running droplet instance.
// It will monitor itself and update health and stats every second.
// TODO: To support multiple backend types this could be made an interface.
type Backend struct {
	Droplet
	Started time.Time
	rt      *statRT

	healthClient *http.Client
	closeMonitor chan struct{}
	Stats        Stats
}

// NewBackEnd returns a Backend configured with the
// Droplet information.
func NewBackEnd(d Droplet, bec BackendConfig) *Backend {
	b := &Backend{Droplet:d}
	// Create a transport that is used for health checks.
	to, _ := time.ParseDuration(bec.HealthTimeout)
	tr := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: to,
			KeepAlive: 0,
		}).Dial,
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	b.healthClient = &http.Client{Transport: tr}

	// Reset running stats.
	b.Stats.Latency = ewma.NewMovingAverage(float64(bec.LatencyAvg))
	b.Stats.FailureRate = ewma.NewMovingAverage(10)

	// Set up the backend transport.
	tod, _ := time.ParseDuration(bec.DialTimeout)
	tr =  &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			return net.DialTimeout(network, addr, tod)
		},
		Proxy: http.ProxyFromEnvironment,
	}

	b.rt = newStatTP(tr)

	b.closeMonitor = make(chan struct{}, 0)
	go b.startMonitor()
	return b
}


// startMonitor will monitor stats of the backend
// Will at times require BOTH rt and Stats mutex.
// This means that no other goroutine should acquire
// both at the same time.
func (b *Backend) startMonitor() {
	s := b.rt
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	exit := shutdown.First()
	end := b.closeMonitor
	previous := time.Now()
	for {
		select {
		case <-ticker.C:
			elapsed := time.Now().Sub(previous)
			previous = time.Now()
			s.mu.Lock()
			b.Stats.mu.Lock()
			if s.requests == 0 {
				b.Stats.Latency.Add(0)
				b.Stats.FailureRate.Add(0)
			} else {
				b.Stats.Latency.Add(float64(s.latencySum) / float64(elapsed) / float64(s.requests))
				b.Stats.FailureRate.Add(float64(s.errors) / float64(s.requests))
			}
			s.requests = 0
			s.errors = 0
			s.latencySum = 0
			s.mu.Unlock()

			// Perform health check
			b.healthCheck()

			if b.Stats.Healthy && b.Stats.healthFailures > 5 {
				log.Println("5 Consequtive health tests failed. Marking as unhealty.")
				b.Stats.Healthy = false
			}
			if !b.Stats.Healthy && b.Stats.healthFailures == 0 {
				log.Println("Health check succeeded. Marking as healty")
				b.Stats.Healthy = true
			}
			b.Stats.mu.Unlock()
		case <-end:
		    exit.Cancel()
			return
		case n := <-exit:
			close(n)
			return
		}
	}
}

// healthCheck will check the health by connecting
// to the healthURL of the backend.
// This is called by healthCheck every second.
// It assumes b.Stats.mu is locked, but will unlock it while
// the request is running.
func (b *Backend) healthCheck() {
	// If no checkurl har been set, assume we are healthy
	if b.HealthURL == "" {
		b.Stats.Healthy = true
		return
	}

	b.Stats.mu.Unlock()
	// Perform the check
	resp, err := b.healthClient.Get(b.HealthURL)

	b.Stats.mu.Lock()
	// Check response
	if err != nil {
		b.Stats.healthFailures++
		log.Println("Error checking health of", b.HealthURL, "Error:", err)
		return
	}
	if resp.StatusCode >= 500 {
		b.Stats.healthFailures++
		log.Println("Error checking health of", b.HealthURL, "Status code:", resp.StatusCode)
	} else {
		// Reset failures
		b.Stats.healthFailures = 0
	}
	resp.Body.Close()
}

// Close the backend, which will shut down monitoring
// of the backend.
func (b *Backend) Close() {
	close(b.closeMonitor)
}

// Host returns the host address of the backend.
func (b Backend) Host() string {
	return b.ServerHost
}

// Connections returns the number of currently running requests.
// Does not include websocket connections.
func (b Backend) Connections() (int) {
	b.rt.mu.RLock()
	n := b.rt.running
	b.rt.mu.RUnlock()
	return n
}


func (s *statRT) RoundTrip(req *http.Request) (*http.Response, error) {
	// Record this request as running
	s.mu.Lock()
	s.running++
	s.mu.Unlock()

	// Time the request roundtrip time
	start := time.Now()
	resp, err := s.rt.RoundTrip(req)
	dur := start.Sub(time.Now())

	// Update stats
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running--
	s.requests++
	s.latencySum += dur
	if err != nil {
		s.errors++
		return nil, err
	}
	// Any status code above or equal to 500 is recorded as an error.
	if resp.StatusCode >= 500 {
		s.errors++
		return resp, nil
	}
	return resp, nil
}

// Transport returns a RoundTripper that will collect stats
// about the backend.
func (b Backend) Transport() http.RoundTripper {
	return b.rt
}

// Healthy returns the healthy state of the backend
func (b Backend) Healthy() bool {
	b.Stats.mu.RLock()
	ok := b.Stats.Healthy
	b.Stats.mu.RUnlock()
	return ok
}

// Stats contain regularly updated statistics about a
// backend. To access be sure to hold the 'mu' mutex.
type Stats struct {
	mu             sync.RWMutex
	healthFailures int // Number of total health check failures
	Healthy        bool
	Latency        ewma.MovingAverage
	FailureRate    ewma.MovingAverage
}

// statRT wraps a http.RoundTripper around statistics that can
// be used for load balancing.
type statRT struct {
	rt         http.RoundTripper
	mu         sync.RWMutex
	latencySum time.Duration
	running    int
	requests   int
	errors     int
}

func newStatTP(rt http.RoundTripper) *statRT {
	s := &statRT{rt: rt}
	return s
}
