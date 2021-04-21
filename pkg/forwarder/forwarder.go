// Copyright Contributors to the Open Cluster Management project

package forwarder

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/kit/log"

	"github.com/prometheus/client_golang/prometheus"
	clientmodel "github.com/prometheus/client_model/go"

	metricshttp "github.com/open-cluster-management/metrics-collector/pkg/http"
	rlogger "github.com/open-cluster-management/metrics-collector/pkg/logger"
	"github.com/open-cluster-management/metrics-collector/pkg/metricfamily"
	"github.com/open-cluster-management/metrics-collector/pkg/metricsclient"
	"github.com/open-cluster-management/metrics-collector/pkg/status"
)

const (
	failedStatusReportMsg = "Failed to report status"
)

var (
	gaugeFederateSamples = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "federate_samples",
		Help: "Tracks the number of samples per federation",
	})
	gaugeFederateFilteredSamples = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "federate_filtered_samples",
		Help: "Tracks the number of samples filtered per federation",
	})
	gaugeFederateErrors = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "federate_errors",
		Help: "The number of times forwarding federated metrics has failed",
	})
)

type RuleMatcher interface {
	MatchRules() []string
}

func init() {
	prometheus.MustRegister(
		gaugeFederateErrors, gaugeFederateSamples, gaugeFederateFilteredSamples,
	)
}

// Config defines the parameters that can be used to configure a worker.
// The only required field is `From`.
type Config struct {
	From          *url.URL
	ToUpload      *url.URL
	FromToken     string
	FromTokenFile string
	FromCAFile    string

	AnonymizeLabels   []string
	AnonymizeSalt     string
	AnonymizeSaltFile string
	Debug             bool
	Interval          time.Duration
	LimitBytes        int64
	Rules             []string
	RulesFile         string
	Transformer       metricfamily.Transformer

	Logger log.Logger
}

// Worker represents a metrics forwarding agent. It collects metrics from a source URL and forwards them to a sink.
// A Worker should be configured with a `Config` and instantiated with the `New` func.
// Workers are thread safe; all access to shared fields are synchronized.
type Worker struct {
	fromClient *metricsclient.Client
	toClient   *metricsclient.Client
	from       *url.URL
	to         *url.URL

	interval    time.Duration
	transformer metricfamily.Transformer
	rules       []string

	lastMetrics []*clientmodel.MetricFamily
	lock        sync.Mutex
	reconfigure chan struct{}

	logger log.Logger

	status status.StatusReport
}

func createClients(cfg Config, interval time.Duration,
	logger log.Logger) (*metricsclient.Client, *metricsclient.Client, metricfamily.MultiTransformer, error) {

	var transformer metricfamily.MultiTransformer

	// Configure the anonymization.
	anonymizeSalt := cfg.AnonymizeSalt
	if len(cfg.AnonymizeSalt) == 0 && len(cfg.AnonymizeSaltFile) > 0 {
		data, err := ioutil.ReadFile(cfg.AnonymizeSaltFile)
		if err != nil {
			return nil, nil, transformer, fmt.Errorf("failed to read anonymize-salt-file: %v", err)
		}
		anonymizeSalt = strings.TrimSpace(string(data))
	}
	if len(cfg.AnonymizeLabels) != 0 && len(anonymizeSalt) == 0 {
		return nil, nil, transformer, fmt.Errorf("anonymize-salt must be specified if anonymize-labels is set")
	}
	if len(cfg.AnonymizeLabels) == 0 {
		rlogger.Log(logger, rlogger.Warn, "msg", "not anonymizing any labels")
	}

	// Configure a transformer.
	if cfg.Transformer != nil {
		transformer.With(cfg.Transformer)
	}
	if len(cfg.AnonymizeLabels) > 0 {
		transformer.With(metricfamily.NewMetricsAnonymizer(anonymizeSalt, cfg.AnonymizeLabels, nil))
	}

	fromTransport := metricsclient.DefaultTransport(logger, false)
	if len(cfg.FromCAFile) > 0 {
		if fromTransport.TLSClientConfig == nil {
			fromTransport.TLSClientConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
			}
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, nil, transformer, fmt.Errorf("failed to read system certificates: %v", err)
		}
		data, err := ioutil.ReadFile(cfg.FromCAFile)
		if err != nil {
			return nil, nil, transformer, fmt.Errorf("failed to read from-ca-file: %v", err)
		}
		if !pool.AppendCertsFromPEM(data) {
			rlogger.Log(logger, rlogger.Warn, "msg", "no certs found in from-ca-file")
		}
		fromTransport.TLSClientConfig.RootCAs = pool
	}

	// Create the `fromClient`.
	fromClient := &http.Client{Transport: fromTransport}
	if cfg.Debug {
		fromClient.Transport = metricshttp.NewDebugRoundTripper(logger, fromClient.Transport)
	}
	if len(cfg.FromToken) == 0 && len(cfg.FromTokenFile) > 0 {
		data, err := ioutil.ReadFile(cfg.FromTokenFile)
		if err != nil {
			return nil, nil, transformer, fmt.Errorf("unable to read from-token-file: %v", err)
		}
		cfg.FromToken = strings.TrimSpace(string(data))
	}
	if len(cfg.FromToken) > 0 {
		fromClient.Transport = metricshttp.NewBearerRoundTripper(cfg.FromToken, fromClient.Transport)
	}
	from := metricsclient.New(logger, fromClient, cfg.LimitBytes, interval, "federate_from")

	// Create the `toClient`.

	toTransport, err := metricsclient.MTLSTransport(logger)
	if err != nil {
		return nil, nil, transformer, errors.New(err.Error())
	}
	toTransport.Proxy = http.ProxyFromEnvironment
	toClient := &http.Client{Transport: toTransport}
	if cfg.Debug {
		toClient.Transport = metricshttp.NewDebugRoundTripper(logger, toClient.Transport)
	}
	to := metricsclient.New(logger, toClient, cfg.LimitBytes, interval, "federate_to")
	return from, to, transformer, nil
}

// New creates a new Worker based on the provided Config. If the Config contains invalid
// values, then an error is returned.
func New(cfg Config) (*Worker, error) {
	if cfg.From == nil {
		return nil, errors.New("a URL from which to scrape is required")
	}
	logger := log.With(cfg.Logger, "component", "forwarder")
	rlogger.Log(logger, rlogger.Warn, "msg", cfg.ToUpload)
	w := Worker{
		from:        cfg.From,
		interval:    cfg.Interval,
		reconfigure: make(chan struct{}),
		to:          cfg.ToUpload,
		logger:      log.With(cfg.Logger, "component", "forwarder/worker"),
	}

	if w.interval == 0 {
		w.interval = 4*time.Minute + 30*time.Second
	}

	fromClient, toClient, transformer, err := createClients(cfg, w.interval, logger)
	if err != nil {
		return nil, err
	}
	w.fromClient = fromClient
	w.toClient = toClient
	w.transformer = transformer

	// Configure the matching rules.
	rules := cfg.Rules
	if len(cfg.RulesFile) > 0 {
		data, err := ioutil.ReadFile(cfg.RulesFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read match-file: %v", err)
		}
		rules = append(rules, strings.Split(string(data), "\n")...)
	}
	for i := 0; i < len(rules); {
		s := strings.TrimSpace(rules[i])
		if len(s) == 0 {
			rules = append(rules[:i], rules[i+1:]...)
			continue
		}
		rules[i] = s
		i++
	}
	w.rules = rules

	s, err := status.New(logger)
	if err != nil {
		return nil, fmt.Errorf("unable to create StatusReport: %v", err)
	}
	w.status = *s

	return &w, nil
}

// Reconfigure temporarily stops a worker and reconfigures is with the provided Config.
// Is thread safe and can run concurrently with `LastMetrics` and `Run`.
func (w *Worker) Reconfigure(cfg Config) error {
	worker, err := New(cfg)
	if err != nil {
		return fmt.Errorf("failed to reconfigure: %v", err)
	}

	w.lock.Lock()
	defer w.lock.Unlock()

	w.fromClient = worker.fromClient
	w.toClient = worker.toClient
	w.interval = worker.interval
	w.from = worker.from
	w.to = worker.to
	w.transformer = worker.transformer
	w.rules = worker.rules

	// Signal a restart to Run func.
	// Do this in a goroutine since we do not care if restarting the Run loop is asynchronous.
	go func() { w.reconfigure <- struct{}{} }()
	return nil
}

func (w *Worker) LastMetrics() []*clientmodel.MetricFamily {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.lastMetrics
}

func (w *Worker) Run(ctx context.Context) {
	for {
		// Ensure that the Worker does not access critical configuration during a reconfiguration.
		w.lock.Lock()
		wait := w.interval
		// The critical section ends here.
		w.lock.Unlock()

		if err := w.forward(ctx); err != nil {
			gaugeFederateErrors.Inc()
			rlogger.Log(w.logger, rlogger.Error, "msg", "unable to forward results", "err", err)
			wait = time.Minute
		}

		select {
		// If the context is cancelled, then we're done.
		case <-ctx.Done():
			return
		case <-time.After(wait):
		// We want to be able to interrupt a sleep to immediately apply a new configuration.
		case <-w.reconfigure:
		}
	}
}

func (w *Worker) forward(ctx context.Context) error {
	w.lock.Lock()
	defer w.lock.Unlock()

	// Load the match rules each time.
	from := w.from

	// reset query from last invocation, otherwise match rules will be appended
	w.from.RawQuery = ""
	v := from.Query()
	for _, rule := range w.rules {
		v.Add("match[]", rule)
	}
	from.RawQuery = v.Encode()

	req := &http.Request{Method: "GET", URL: from}
	start := time.Now()
	families, err := w.fromClient.Retrieve(ctx, req)
	if err != nil {
		statusErr := w.status.UpdateStatus("Degraded", "Degraded", "Failed to retrieve metrics")
		if statusErr != nil {
			rlogger.Log(w.logger, rlogger.Warn, "msg", failedStatusReportMsg, "err", err)
		}
		return err
	}
	elapsed := time.Since(start)
	rlogger.Log(w.logger, rlogger.Info, "took time for federate", elapsed)

	start = time.Now()
	families1, err := w.fromClient.Retrieve1(ctx)
	if err != nil {
		rlogger.Log(w.logger, rlogger.Warn, "msg", "Failed to get recording", "err", err)
	} else {
		families = append(families, families1...)
	}
	elapsed = time.Since(start)
	rlogger.Log(w.logger, rlogger.Info, "took time for query", elapsed)

	before := metricfamily.MetricsCount(families)
	if err := metricfamily.Filter(families, w.transformer); err != nil {
		statusErr := w.status.UpdateStatus("Degraded", "Degraded", "Failed to filter metrics")
		if statusErr != nil {
			rlogger.Log(w.logger, rlogger.Warn, "msg", failedStatusReportMsg, "err", err)
		}
		return err
	}

	families = metricfamily.Pack(families)
	after := metricfamily.MetricsCount(families)

	gaugeFederateSamples.Set(float64(before))
	gaugeFederateFilteredSamples.Set(float64(before - after))

	w.lastMetrics = families

	if len(families) == 0 {
		rlogger.Log(w.logger, rlogger.Warn, "msg", "no metrics to send, doing nothing")
		statusErr := w.status.UpdateStatus("Available", "Available", "No metrics to send")
		if statusErr != nil {
			rlogger.Log(w.logger, rlogger.Warn, "msg", failedStatusReportMsg, "err", err)
		}
		return nil
	}

	if w.to == nil {
		rlogger.Log(w.logger, rlogger.Warn, "msg", "to is nil, doing nothing")
		statusErr := w.status.UpdateStatus("Available", "Available", "Metrics is not required to send")
		if statusErr != nil {
			rlogger.Log(w.logger, rlogger.Warn, "msg", failedStatusReportMsg, "err", err)
		}
		return nil
	}

	req = &http.Request{Method: "POST", URL: w.to}
	err = w.toClient.RemoteWrite(ctx, req, families, w.interval)
	if err != nil {
		statusErr := w.status.UpdateStatus("Degraded", "Degraded", "Failed to send metrics")
		if statusErr != nil {
			rlogger.Log(w.logger, rlogger.Warn, "msg", failedStatusReportMsg, "err", err)
		}
	} else {
		statusErr := w.status.UpdateStatus("Available", "Available", "Send metrics successfully")
		if statusErr != nil {
			rlogger.Log(w.logger, rlogger.Warn, "msg", failedStatusReportMsg, "err", err)
		}
	}
	return err
}
