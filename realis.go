/**
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package realis provides the ability to use Thrift API to communicate with Apache Aurora.
package realis

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/paypal/gorealis/gen-go/apache/aurora"
	"github.com/paypal/gorealis/response"
	"github.com/pkg/errors"
)

const VERSION = "1.21.0"

// TODO(rdelvalle): Move documentation to interface in order to make godoc look better/more accessible
type Realis interface {
	AbortJobUpdate(updateKey aurora.JobUpdateKey, message string) (*aurora.Response, error)
	AddInstances(instKey aurora.InstanceKey, count int32) (*aurora.Response, error)
	CreateJob(auroraJob Job) (*aurora.Response, error)
	CreateService(auroraJob Job, settings *aurora.JobUpdateSettings) (*aurora.Response, *aurora.StartJobUpdateResult_, error)
	DescheduleCronJob(key *aurora.JobKey) (*aurora.Response, error)
	FetchTaskConfig(instKey aurora.InstanceKey) (*aurora.TaskConfig, error)
	GetInstanceIds(key *aurora.JobKey, states []aurora.ScheduleStatus) ([]int32, error)
	GetJobUpdateSummaries(jobUpdateQuery *aurora.JobUpdateQuery) (*aurora.Response, error)
	GetTaskStatus(query *aurora.TaskQuery) ([]*aurora.ScheduledTask, error)
	GetTasksWithoutConfigs(query *aurora.TaskQuery) ([]*aurora.ScheduledTask, error)
	GetJobs(role string) (*aurora.Response, *aurora.GetJobsResult_, error)
	GetPendingReason(query *aurora.TaskQuery) (pendingReasons []*aurora.PendingReason, e error)
	JobUpdateDetails(updateQuery aurora.JobUpdateQuery) (*aurora.Response, error)
	KillJob(key *aurora.JobKey) (*aurora.Response, error)
	KillInstances(key *aurora.JobKey, instances ...int32) (*aurora.Response, error)
	RemoveInstances(key *aurora.JobKey, count int32) (*aurora.Response, error)
	RestartInstances(key *aurora.JobKey, instances ...int32) (*aurora.Response, error)
	RestartJob(key *aurora.JobKey) (*aurora.Response, error)
	RollbackJobUpdate(key aurora.JobUpdateKey, message string) (*aurora.Response, error)
	ScheduleCronJob(auroraJob Job) (*aurora.Response, error)
	StartJobUpdate(updateJob *UpdateJob, message string) (*aurora.Response, error)

	PauseJobUpdate(key *aurora.JobUpdateKey, message string) (*aurora.Response, error)
	ResumeJobUpdate(key *aurora.JobUpdateKey, message string) (*aurora.Response, error)
	PulseJobUpdate(key *aurora.JobUpdateKey) (*aurora.Response, error)
	StartCronJob(key *aurora.JobKey) (*aurora.Response, error)
	// TODO: Remove this method and make it private to avoid race conditions
	ReestablishConn() error
	RealisConfig() *RealisConfig
	Close()

	// Admin functions
	DrainHosts(hosts ...string) (*aurora.Response, *aurora.DrainHostsResult_, error)
	SLADrainHosts(policy *aurora.SlaPolicy, timeout int64, hosts ...string) (*aurora.DrainHostsResult_, error)
	StartMaintenance(hosts ...string) (*aurora.Response, *aurora.StartMaintenanceResult_, error)
	EndMaintenance(hosts ...string) (*aurora.Response, *aurora.EndMaintenanceResult_, error)
	MaintenanceStatus(hosts ...string) (*aurora.Response, *aurora.MaintenanceStatusResult_, error)
	SetQuota(role string, cpu *float64, ram *int64, disk *int64) (*aurora.Response, error)
	GetQuota(role string) (*aurora.Response, error)
	Snapshot() error
	PerformBackup() error
	// Force an Implicit reconciliation between Mesos and Aurora
	ForceImplicitTaskReconciliation() error
	// Force an Explicit reconciliation between Mesos and Aurora
	ForceExplicitTaskReconciliation(batchSize *int32) error
}

type realisClient struct {
	config         *RealisConfig
	client         *aurora.AuroraSchedulerManagerClient
	readonlyClient *aurora.ReadOnlySchedulerClient
	adminClient    *aurora.AuroraAdminClient
	logger         LevelLogger
	lock           *sync.Mutex
	debug          bool
	transport      thrift.TTransport
}

type RealisConfig struct {
	username, password          string
	url                         string
	timeoutms                   int
	binTransport, jsonTransport bool
	cluster                     *Cluster
	backoff                     Backoff
	transport                   thrift.TTransport
	protoFactory                thrift.TProtocolFactory
	logger                      *LevelLogger
	InsecureSkipVerify          bool
	certspath                   string
	clientKey, clientCert       string
	options                     []ClientOption
	debug                       bool
	trace                       bool
	zkOptions                   []ZKOpt
}

var defaultBackoff = Backoff{
	Steps:    3,
	Duration: 10 * time.Second,
	Factor:   5.0,
	Jitter:   0.1,
}

const APIPath = "/api"

type ClientOption func(*RealisConfig)

//Config sets for options in RealisConfig.
func BasicAuth(username, password string) ClientOption {
	return func(config *RealisConfig) {
		config.username = username
		config.password = password
	}
}

func SchedulerUrl(url string) ClientOption {
	return func(config *RealisConfig) {
		config.url = url
	}
}

func TimeoutMS(timeout int) ClientOption {
	return func(config *RealisConfig) {
		config.timeoutms = timeout
	}
}

func ZKCluster(cluster *Cluster) ClientOption {
	return func(config *RealisConfig) {
		config.cluster = cluster
	}
}

func ZKUrl(url string) ClientOption {

	opts := []ZKOpt{ZKEndpoints(strings.Split(url, ",")...), ZKPath("/aurora/scheduler")}

	return func(config *RealisConfig) {
		if config.zkOptions == nil {
			config.zkOptions = opts
		} else {
			config.zkOptions = append(config.zkOptions, opts...)
		}
	}
}

func Retries(backoff Backoff) ClientOption {
	return func(config *RealisConfig) {
		config.backoff = backoff
	}
}

func ThriftJSON() ClientOption {
	return func(config *RealisConfig) {
		config.jsonTransport = true
	}
}

func ThriftBinary() ClientOption {
	return func(config *RealisConfig) {
		config.binTransport = true
	}
}

func BackOff(b Backoff) ClientOption {
	return func(config *RealisConfig) {
		config.backoff = b
	}
}

func InsecureSkipVerify(InsecureSkipVerify bool) ClientOption {
	return func(config *RealisConfig) {
		config.InsecureSkipVerify = InsecureSkipVerify
	}
}

func Certspath(certspath string) ClientOption {
	return func(config *RealisConfig) {
		config.certspath = certspath
	}
}

func ClientCerts(clientKey, clientCert string) ClientOption {
	return func(config *RealisConfig) {
		config.clientKey, config.clientCert = clientKey, clientCert
	}
}

// Use this option if you'd like to override default settings for connecting to Zookeeper.
// See zk.go for what is possible to set as an option.
func ZookeeperOptions(opts ...ZKOpt) ClientOption {
	return func(config *RealisConfig) {
		config.zkOptions = opts
	}
}

// Using the word set to avoid name collision with Interface.
func SetLogger(l Logger) ClientOption {
	return func(config *RealisConfig) {
		config.logger = &LevelLogger{Logger: l}
	}
}

// Enable debug statements.
func Debug() ClientOption {
	return func(config *RealisConfig) {
		config.debug = true
	}
}

// Enable debug statements.
func Trace() ClientOption {
	return func(config *RealisConfig) {
		config.trace = true
	}
}

func newTJSONTransport(url string, timeout int, config *RealisConfig) (thrift.TTransport, error) {
	trans, err := defaultTTransport(url, timeout, config)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create transport")
	}
	httpTrans := (trans).(*thrift.THttpClient)
	httpTrans.SetHeader("Content-Type", "application/x-thrift")
	httpTrans.SetHeader("User-Agent", "gorealis v"+VERSION)
	return trans, err
}

func newTBinTransport(url string, timeout int, config *RealisConfig) (thrift.TTransport, error) {
	trans, err := defaultTTransport(url, timeout, config)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create transport")
	}
	httpTrans := (trans).(*thrift.THttpClient)
	httpTrans.DelHeader("Content-Type") // Workaround for using thrift HttpPostClient
	httpTrans.SetHeader("Accept", "application/vnd.apache.thrift.binary")
	httpTrans.SetHeader("Content-Type", "application/vnd.apache.thrift.binary")
	httpTrans.SetHeader("User-Agent", "gorealis v"+VERSION)

	return trans, err
}

// This client implementation of the realis interface uses a retry mechanism for all Thrift Calls.
// It will retry all calls which result in a temporary failure as well as calls that fail due to an EOF
// being returned by the http client. Most permanent failures are now being caught by the thriftCallWithRetries
// function and not being retried but there may be corner cases not yet handled.
func NewRealisClient(options ...ClientOption) (Realis, error) {
	config := &RealisConfig{}

	// Default configs
	config.timeoutms = 10000
	config.backoff = defaultBackoff
	config.logger = &LevelLogger{Logger: log.New(os.Stdout, "realis: ", log.Ltime|log.Ldate|log.LUTC)}

	// Save options to recreate client if a connection error happens
	config.options = options

	// Override default configs where necessary
	for _, opt := range options {
		opt(config)
	}

	// TODO(rdelvalle): Move this logic to it's own function to make initialization code easier to read.

	// Turn off all logging (including debug)
	if config.logger == nil {
		config.logger = &LevelLogger{Logger: NoopLogger{}}
	}

	// Set a logger if debug has been set to true but no logger has been set
	if config.logger == nil && config.debug {
		config.logger = &LevelLogger{
			Logger: log.New(os.Stdout, "realis: ", log.Ltime|log.Ldate|log.LUTC),
			debug:  true,
		}
	}

	config.logger.debug = config.debug
	config.logger.trace = config.trace

	// Note, by this point, a LevelLogger should have been created.
	config.logger.EnableDebug(config.debug)
	config.logger.EnableTrace(config.trace)

	config.logger.DebugPrintln("Number of options applied to config: ", len(options))

	// Set default Transport to JSON if needed.
	if !config.jsonTransport && !config.binTransport {
		config.jsonTransport = true
	}

	var url string
	var err error

	// Find the leader using custom Zookeeper options if options are provided
	if config.zkOptions != nil {
		url, err = LeaderFromZKOpts(config.zkOptions...)
		if err != nil {
			return nil, NewTemporaryError(errors.Wrap(err, "unable to use zk to get leader"))
		}
		config.logger.Println("Scheduler URL from ZK: ", url)
	} else if config.cluster != nil {
		// Determine how to get information to connect to the scheduler.
		// Prioritize getting leader from ZK over using a direct URL.
		url, err = LeaderFromZK(*config.cluster)
		// If ZK is configured, throw an error if the leader is unable to be determined
		if err != nil {
			return nil, NewTemporaryError(errors.Wrap(err, "unable to use zk to get leader	"))
		}
		config.logger.Println("Scheduler URL from ZK: ", url)
	} else if config.url != "" {
		url = config.url
		config.logger.Println("Scheduler URL: ", url)
	} else {
		return nil, errors.New("incomplete Options -- url, cluster.json, or Zookeeper address required")
	}

	if config.jsonTransport {
		trans, err := newTJSONTransport(url, config.timeoutms, config)
		if err != nil {
			return nil, NewTemporaryError(err)
		}
		config.transport = trans
		config.protoFactory = thrift.NewTJSONProtocolFactory()

	} else if config.binTransport {
		trans, err := newTBinTransport(url, config.timeoutms, config)
		if err != nil {
			return nil, NewTemporaryError(err)
		}
		config.transport = trans
		config.protoFactory = thrift.NewTBinaryProtocolFactoryDefault()
	}

	config.logger.Printf("gorealis config url: %+v\n", url)

	// Adding Basic Authentication.
	if config.username != "" && config.password != "" {
		httpTrans := (config.transport).(*thrift.THttpClient)
		httpTrans.SetHeader("Authorization", "Basic "+basicAuth(config.username, config.password))
	}

	return &realisClient{
		config:         config,
		client:         aurora.NewAuroraSchedulerManagerClientFactory(config.transport, config.protoFactory),
		readonlyClient: aurora.NewReadOnlySchedulerClientFactory(config.transport, config.protoFactory),
		adminClient:    aurora.NewAuroraAdminClientFactory(config.transport, config.protoFactory),
		logger:         LevelLogger{Logger: config.logger, debug: config.debug, trace: config.trace},
		lock:           &sync.Mutex{},
		transport:      config.transport}, nil
}

func GetDefaultClusterFromZKUrl(zkurl string) *Cluster {
	return &Cluster{
		Name:          "defaultCluster",
		AuthMechanism: "UNAUTHENTICATED",
		ZK:            zkurl,
		SchedZKPath:   "/aurora/scheduler",
		AgentRunDir:   "latest",
		AgentRoot:     "/var/lib/mesos",
	}
}

func GetCerts(certPath string) (*x509.CertPool, error) {
	globalRootCAs := x509.NewCertPool()
	caFiles, err := ioutil.ReadDir(certPath)
	if err != nil {
		return nil, err
	}
	for _, cert := range caFiles {
		caPathFile := filepath.Join(certPath, cert.Name())
		caCert, err := ioutil.ReadFile(caPathFile)
		if err != nil {
			return nil, err
		}
		globalRootCAs.AppendCertsFromPEM(caCert)
	}
	return globalRootCAs, nil
}

// Creates a default Thrift Transport object for communications in gorealis using an HTTP Post Client
func defaultTTransport(url string, timeoutMs int, config *RealisConfig) (thrift.TTransport, error) {
	var transport http.Transport
	if config != nil {
		tlsConfig := &tls.Config{InsecureSkipVerify: config.InsecureSkipVerify}

		if config.certspath != "" {
			rootCAs, err := GetCerts(config.certspath)
			if err != nil {
				config.logger.Println("error occurred couldn't fetch certs")
				return nil, err
			}
			tlsConfig.RootCAs = rootCAs
		}
		if config.clientKey != "" && config.clientCert == "" {
			return nil, fmt.Errorf("have to provide both client key, cert. Only client key provided ")
		}
		if config.clientKey == "" && config.clientCert != "" {
			return nil, fmt.Errorf("have to provide both client key, cert. Only client cert provided ")
		}
		if config.clientKey != "" && config.clientCert != "" {
			cert, err := tls.LoadX509KeyPair(config.clientCert, config.clientKey)
			if err != nil {
				config.logger.Println("error occurred loading client certs and keys")
				return nil, err
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
		transport.TLSClientConfig = tlsConfig
	}

	trans, err := thrift.NewTHttpClientWithOptions(
		url+APIPath,
		thrift.THttpClientOptions{
			Client: &http.Client{
				Timeout:   time.Millisecond * time.Duration(timeoutMs),
				Transport: &transport}})

	if err != nil {
		return nil, errors.Wrap(err, "Error creating transport")
	}

	if err := trans.Open(); err != nil {
		return nil, errors.Wrapf(err, "Error opening connection to %s", url)
	}

	return trans, nil
}

func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

func (r *realisClient) ReestablishConn() error {
	// Close existing connection
	r.logger.Println("Re-establishing Connection to Aurora")
	r.Close()

	r.lock.Lock()
	defer r.lock.Unlock()

	// Recreate connection from scratch using original options
	newRealis, err := NewRealisClient(r.config.options...)
	if err != nil {
		// This could be a temporary network hiccup
		return NewTemporaryError(err)
	}

	// If we are able to successfully re-connect, make receiver
	// point to newly established connections.
	if newClient, ok := newRealis.(*realisClient); ok {
		r.config = newClient.config
		r.client = newClient.client
		r.readonlyClient = newClient.readonlyClient
		r.adminClient = newClient.adminClient
		r.logger = newClient.logger
	}

	return nil
}

// Releases resources associated with the realis client.
func (r *realisClient) Close() {

	r.lock.Lock()
	defer r.lock.Unlock()

	r.transport.Close()
}

// Uses predefined set of states to retrieve a set of active jobs in Apache Aurora.
func (r *realisClient) GetInstanceIds(key *aurora.JobKey, states []aurora.ScheduleStatus) ([]int32, error) {
	taskQ := &aurora.TaskQuery{
		JobKeys:  []*aurora.JobKey{{Environment: key.Environment, Role: key.Role, Name: key.Name}},
		Statuses: states,
	}

	r.logger.DebugPrintf("GetTasksWithoutConfigs Thrift Payload: %+v\n", taskQ)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.GetTasksWithoutConfigs(nil, taskQ)
		})

	// If we encountered an error we couldn't recover from by retrying, return an error to the user
	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error querying Aurora Scheduler for active IDs")
	}

	// Construct instance id map to stay in line with thrift's representation of sets
	tasks := response.ScheduleStatusResult(resp).GetTasks()
	jobInstanceIds := make([]int32, 0, len(tasks))
	for _, task := range tasks {
		jobInstanceIds = append(jobInstanceIds, task.GetAssignedTask().GetInstanceId())
	}
	return jobInstanceIds, nil

}

func (r *realisClient) GetJobUpdateSummaries(jobUpdateQuery *aurora.JobUpdateQuery) (*aurora.Response, error) {

	r.logger.DebugPrintf("GetJobUpdateSummaries Thrift Payload: %+v\n", jobUpdateQuery)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.readonlyClient.GetJobUpdateSummaries(nil, jobUpdateQuery)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error getting job update summaries from Aurora Scheduler")
	}

	return resp, nil
}

func (r *realisClient) GetJobs(role string) (*aurora.Response, *aurora.GetJobsResult_, error) {

	var result *aurora.GetJobsResult_

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.readonlyClient.GetJobs(nil, role)
		})

	if retryErr != nil {
		return nil, result, errors.Wrap(retryErr, "error getting Jobs from Aurora Scheduler")
	}

	if resp.GetResult_() != nil {
		result = resp.GetResult_().GetJobsResult_
	}

	return resp, result, nil
}

// Kill specific instances of a job.
func (r *realisClient) KillInstances(key *aurora.JobKey, instances ...int32) (*aurora.Response, error) {
	r.logger.DebugPrintf("KillTasks Thrift Payload: %+v %v\n", key, instances)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.KillTasks(nil, key, instances, "")
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending Kill command to Aurora Scheduler")
	}
	return resp, nil
}

func (r *realisClient) RealisConfig() *RealisConfig {
	return r.config
}

// Sends a kill message to the scheduler for all active tasks under a job.
func (r *realisClient) KillJob(key *aurora.JobKey) (*aurora.Response, error) {

	r.logger.DebugPrintf("KillTasks Thrift Payload: %+v\n", key)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			// Giving the KillTasks thrift call an empty set tells the Aurora scheduler to kill all active shards
			return r.client.KillTasks(nil, key, nil, "")
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending Kill command to Aurora Scheduler")
	}
	return resp, nil
}

// Sends a create job message to the scheduler with a specific job configuration.
// Although this API is able to create service jobs, it is better to use CreateService instead
// as that API uses the update thrift call which has a few extra features available.
// Use this API to create ad-hoc jobs.
func (r *realisClient) CreateJob(auroraJob Job) (*aurora.Response, error) {

	r.logger.DebugPrintf("CreateJob Thrift Payload: %+v\n", auroraJob.JobConfig())

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.CreateJob(nil, auroraJob.JobConfig())
		})

	if retryErr != nil {
		return resp, errors.Wrap(retryErr, "error sending Create command to Aurora Scheduler")
	}
	return resp, nil
}

// This API uses an update thrift call to create the services giving a few more robust features.
func (r *realisClient) CreateService(auroraJob Job, settings *aurora.JobUpdateSettings) (*aurora.Response, *aurora.StartJobUpdateResult_, error) {
	// Create a new job update object and ship it to the StartJobUpdate api
	update := NewUpdateJob(auroraJob.TaskConfig(), settings)
	update.InstanceCount(auroraJob.GetInstanceCount())

	resp, err := r.StartJobUpdate(update, "")
	if err != nil {
		if IsTimeout(err) {
			return resp, nil, err
		}

		return resp, nil, errors.Wrap(err, "unable to create service")
	}

	if resp.GetResult_() != nil {
		return resp, resp.GetResult_().GetStartJobUpdateResult_(), nil
	}

	return nil, nil, errors.New("results object is nil")
}

func (r *realisClient) ScheduleCronJob(auroraJob Job) (*aurora.Response, error) {
	r.logger.DebugPrintf("ScheduleCronJob Thrift Payload: %+v\n", auroraJob.JobConfig())

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.ScheduleCronJob(nil, auroraJob.JobConfig())
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending Cron Job Schedule message to Aurora Scheduler")
	}
	return resp, nil
}

func (r *realisClient) DescheduleCronJob(key *aurora.JobKey) (*aurora.Response, error) {

	r.logger.DebugPrintf("DescheduleCronJob Thrift Payload: %+v\n", key)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.DescheduleCronJob(nil, key)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending Cron Job De-schedule message to Aurora Scheduler")

	}
	return resp, nil

}

func (r *realisClient) StartCronJob(key *aurora.JobKey) (*aurora.Response, error) {

	r.logger.DebugPrintf("StartCronJob Thrift Payload: %+v\n", key)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.StartCronJob(nil, key)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending Start Cron Job  message to Aurora Scheduler")
	}
	return resp, nil

}

// Restarts specific instances specified
func (r *realisClient) RestartInstances(key *aurora.JobKey, instances ...int32) (*aurora.Response, error) {
	r.logger.DebugPrintf("RestartShards Thrift Payload: %+v %v\n", key, instances)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.RestartShards(nil, key, instances)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending Restart command to Aurora Scheduler")
	}
	return resp, nil
}

// Restarts all active tasks under a job configuration.
func (r *realisClient) RestartJob(key *aurora.JobKey) (*aurora.Response, error) {

	instanceIds, err1 := r.GetInstanceIds(key, aurora.ACTIVE_STATES)
	if err1 != nil {
		return nil, errors.Wrap(err1, "Could not retrieve relevant task instance IDs")
	}

	r.logger.DebugPrintf("RestartShards Thrift Payload: %+v %v\n", key, instanceIds)

	if len(instanceIds) > 0 {
		resp, retryErr := r.thriftCallWithRetries(
			false,
			func() (*aurora.Response, error) {
				return r.client.RestartShards(nil, key, instanceIds)
			})

		if retryErr != nil {
			return nil, errors.Wrap(retryErr, "error sending Restart command to Aurora Scheduler")
		}

		return resp, nil
	} else {
		return nil, errors.New("No tasks in the Active state")
	}
}

// Update all tasks under a job configuration. Currently gorealis doesn't support for canary deployments.
func (r *realisClient) StartJobUpdate(updateJob *UpdateJob, message string) (*aurora.Response, error) {

	r.logger.DebugPrintf("StartJobUpdate Thrift Payload: %+v %v\n", updateJob, message)

	resp, retryErr := r.thriftCallWithRetries(
		true,
		func() (*aurora.Response, error) {
			return r.client.StartJobUpdate(nil, updateJob.req, message)
		})

	if retryErr != nil {
		// A timeout took place when attempting this call, attempt to recover
		if IsTimeout(retryErr) {
			return resp, retryErr
		} else {
			return resp, errors.Wrap(retryErr, "error sending StartJobUpdate command to Aurora Scheduler")
		}
	}
	return resp, nil
}

// Abort Job Update on Aurora. Requires the updateId which can be obtained on the Aurora web UI.
// This API is meant to be synchronous. It will attempt to wait until the update transitions to the aborted state.
// However, if the job update does not transition to the ABORT state an error will be returned.
func (r *realisClient) AbortJobUpdate(updateKey aurora.JobUpdateKey, message string) (*aurora.Response, error) {

	r.logger.DebugPrintf("AbortJobUpdate Thrift Payload: %+v %v\n", updateKey, message)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.AbortJobUpdate(nil, &updateKey, message)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending AbortJobUpdate command to Aurora Scheduler")
	}

	// Make this call synchronous by  blocking until it job has successfully transitioned to aborted
	m := Monitor{Client: r}
	_, err := m.JobUpdateStatus(updateKey, map[aurora.JobUpdateStatus]bool{aurora.JobUpdateStatus_ABORTED: true}, time.Second*5, time.Minute)

	return resp, err
}

// Pause Job Update. UpdateID is returned from StartJobUpdate or the Aurora web UI.
func (r *realisClient) PauseJobUpdate(updateKey *aurora.JobUpdateKey, message string) (*aurora.Response, error) {

	r.logger.DebugPrintf("PauseJobUpdate Thrift Payload: %+v %v\n", updateKey, message)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.PauseJobUpdate(nil, updateKey, message)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending PauseJobUpdate command to Aurora Scheduler")
	}

	return resp, nil
}

// Resume Paused Job Update. UpdateID is returned from StartJobUpdate or the Aurora web UI.
func (r *realisClient) ResumeJobUpdate(updateKey *aurora.JobUpdateKey, message string) (*aurora.Response, error) {

	r.logger.DebugPrintf("ResumeJobUpdate Thrift Payload: %+v %v\n", updateKey, message)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.ResumeJobUpdate(nil, updateKey, message)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending ResumeJobUpdate command to Aurora Scheduler")
	}

	return resp, nil
}

// Pulse Job Update on Aurora. UpdateID is returned from StartJobUpdate or the Aurora web UI.
func (r *realisClient) PulseJobUpdate(updateKey *aurora.JobUpdateKey) (*aurora.Response, error) {

	r.logger.DebugPrintf("PulseJobUpdate Thrift Payload: %+v\n", updateKey)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.PulseJobUpdate(nil, updateKey)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending PulseJobUpdate command to Aurora Scheduler")
	}

	return resp, nil
}

// Scale up the number of instances under a job configuration using the configuration for specific
// instance to scale up.
func (r *realisClient) AddInstances(instKey aurora.InstanceKey, count int32) (*aurora.Response, error) {

	r.logger.DebugPrintf("AddInstances Thrift Payload: %+v %v\n", instKey, count)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.AddInstances(nil, &instKey, count)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error sending AddInstances command to Aurora Scheduler")
	}
	return resp, nil

}

// Scale down the number of instances under a job configuration using the configuration of a specific instance
func (r *realisClient) RemoveInstances(key *aurora.JobKey, count int32) (*aurora.Response, error) {
	instanceIds, err := r.GetInstanceIds(key, aurora.ACTIVE_STATES)
	if err != nil {
		return nil, errors.Wrap(err, "RemoveInstances: Could not retrieve relevant instance IDs")
	}

	if len(instanceIds) < int(count) {
		return nil, errors.Errorf("Insufficient active instances available for killing: "+
			" Instances to be killed %d Active instances %d", count, len(instanceIds))
	}

	// Sort instanceIds in ** decreasing ** order
	sort.Slice(instanceIds, func(i, j int) bool {
		return instanceIds[i] > instanceIds[j]
	})

	// Kill the instances with the highest ID number first
	return r.KillInstances(key, instanceIds[:count]...)
}

// Get information about task including a fully hydrated task configuration object
func (r *realisClient) GetTaskStatus(query *aurora.TaskQuery) ([]*aurora.ScheduledTask, error) {

	r.logger.DebugPrintf("GetTasksStatus Thrift Payload: %+v\n", query)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.GetTasksStatus(nil, query)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error querying Aurora Scheduler for task status")
	}

	return response.ScheduleStatusResult(resp).GetTasks(), nil
}

// Get pending reason
func (r *realisClient) GetPendingReason(query *aurora.TaskQuery) ([]*aurora.PendingReason, error) {

	r.logger.DebugPrintf("GetPendingReason Thrift Payload: %+v\n", query)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.GetPendingReason(nil, query)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error querying Aurora Scheduler for pending Reasons")
	}

	var pendingReasons []*aurora.PendingReason

	if resp.GetResult_() != nil {
		pendingReasons = resp.GetResult_().GetGetPendingReasonResult_().GetReasons()
	}

	return pendingReasons, nil
}

// Get information about task including without a task configuration object
func (r *realisClient) GetTasksWithoutConfigs(query *aurora.TaskQuery) ([]*aurora.ScheduledTask, error) {

	r.logger.DebugPrintf("GetTasksWithoutConfigs Thrift Payload: %+v\n", query)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.GetTasksWithoutConfigs(nil, query)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error querying Aurora Scheduler for task status without configs")
	}

	return response.ScheduleStatusResult(resp).GetTasks(), nil

}

// Get the task configuration from the aurora scheduler for a job
func (r *realisClient) FetchTaskConfig(instKey aurora.InstanceKey) (*aurora.TaskConfig, error) {
	taskQ := &aurora.TaskQuery{
		Role:        &instKey.JobKey.Role,
		Environment: &instKey.JobKey.Environment,
		JobName:     &instKey.JobKey.Name,
		InstanceIds: []int32{instKey.InstanceId},
		Statuses:    aurora.ACTIVE_STATES,
	}

	r.logger.DebugPrintf("GetTasksStatus Thrift Payload: %+v\n", taskQ)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.GetTasksStatus(nil, taskQ)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "error querying Aurora Scheduler for task configuration")
	}

	tasks := response.ScheduleStatusResult(resp).GetTasks()

	if len(tasks) == 0 {
		return nil, errors.Errorf("Instance %d for jobkey %s/%s/%s doesn't exist",
			instKey.InstanceId,
			instKey.JobKey.Environment,
			instKey.JobKey.Role,
			instKey.JobKey.Name)
	}

	// Currently, instance 0 is always picked..
	return tasks[0].AssignedTask.Task, nil
}

func (r *realisClient) JobUpdateDetails(updateQuery aurora.JobUpdateQuery) (*aurora.Response, error) {

	r.logger.DebugPrintf("GetJobUpdateDetails Thrift Payload: %+v\n", updateQuery)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.GetJobUpdateDetails(nil, &updateQuery)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "Unable to get job update details")
	}
	return resp, nil

}

func (r *realisClient) RollbackJobUpdate(key aurora.JobUpdateKey, message string) (*aurora.Response, error) {

	r.logger.DebugPrintf("RollbackJobUpdate Thrift Payload: %+v %v\n", key, message)

	resp, retryErr := r.thriftCallWithRetries(
		false,
		func() (*aurora.Response, error) {
			return r.client.RollbackJobUpdate(nil, &key, message)
		})

	if retryErr != nil {
		return nil, errors.Wrap(retryErr, "unable to roll back job update")
	}
	return resp, nil
}
