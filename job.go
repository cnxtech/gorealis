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

package realis

import (
	"strconv"

	"github.com/paypal/gorealis/gen-go/apache/aurora"
)

type Job interface {
	// Set Job Key environment.
	Environment(env string) Job
	Role(role string) Job
	Name(name string) Job
	CronSchedule(cron string) Job
	CronCollisionPolicy(policy aurora.CronCollisionPolicy) Job
	CPU(cpus float64) Job
	Disk(disk int64) Job
	RAM(ram int64) Job
	GPU(gpu int64) Job
	ExecutorName(name string) Job
	ExecutorData(data string) Job
	AddPorts(num int) Job
	AddLabel(key string, value string) Job
	AddNamedPorts(names ...string) Job
	AddLimitConstraint(name string, limit int32) Job
	AddValueConstraint(name string, negated bool, values ...string) Job

	// From Aurora Docs:
	// dedicated attribute. Aurora treats this specially, and only allows matching jobs
	// to run on these machines, and will only schedule matching jobs on these machines.
	// When a job is created, the scheduler requires that the $role component matches
	// the role field in the job configuration, and will reject the job creation otherwise.
	// A wildcard (*) may be used for the role portion of the dedicated attribute, which
	// will allow any owner to elect for a job to run on the host(s)
	AddDedicatedConstraint(role, name string) Job
	AddURIs(extract bool, cache bool, values ...string) Job
	JobKey() *aurora.JobKey
	JobConfig() *aurora.JobConfiguration
	TaskConfig() *aurora.TaskConfig
	IsService(isService bool) Job
	InstanceCount(instCount int32) Job
	GetInstanceCount() int32
	MaxFailure(maxFail int32) Job
	Container(container Container) Job
	PartitionPolicy(policy *aurora.PartitionPolicy) Job
	Tier(tier string) Job
	SlaPolicy(policy *aurora.SlaPolicy) Job
}

type ResourceType int

const (
	CPU ResourceType = iota
	RAM
	DISK
	GPU
)

// Structure to collect all information pertaining to an Aurora job.
type AuroraJob struct {
	jobConfig *aurora.JobConfiguration
	resources map[ResourceType]*aurora.Resource
	metadata  map[string]*aurora.Metadata
	portCount int
}

// Create a Job object with everything initialized.
func NewJob() Job {
	jobConfig := aurora.NewJobConfiguration()
	taskConfig := aurora.NewTaskConfig()
	jobKey := aurora.NewJobKey()

	// Job Config
	jobConfig.Key = jobKey
	jobConfig.TaskConfig = taskConfig

	// Task Config
	taskConfig.Job = jobKey
	taskConfig.Container = aurora.NewContainer()
	taskConfig.Container.Mesos = aurora.NewMesosContainer()

	// Resources
	numCpus := aurora.NewResource()
	ramMb := aurora.NewResource()
	diskMb := aurora.NewResource()

	resources := map[ResourceType]*aurora.Resource{CPU: numCpus, RAM: ramMb, DISK: diskMb}
	taskConfig.Resources = []*aurora.Resource{numCpus, ramMb, diskMb}

	numCpus.NumCpus = new(float64)
	ramMb.RamMb = new(int64)
	diskMb.DiskMb = new(int64)

	return &AuroraJob{
		jobConfig: jobConfig,
		resources: resources,
		metadata:  make(map[string]*aurora.Metadata),
		portCount: 0,
	}
}

// Set Job Key environment.
func (j *AuroraJob) Environment(env string) Job {
	j.jobConfig.Key.Environment = env
	return j
}

// Set Job Key Role.
func (j *AuroraJob) Role(role string) Job {
	j.jobConfig.Key.Role = role

	// Will be deprecated
	identity := &aurora.Identity{User: role}
	j.jobConfig.Owner = identity
	j.jobConfig.TaskConfig.Owner = identity
	return j
}

// Set Job Key Name.
func (j *AuroraJob) Name(name string) Job {
	j.jobConfig.Key.Name = name
	return j
}

// Set name of the executor that will the task will be configured to.
func (j *AuroraJob) ExecutorName(name string) Job {

	if j.jobConfig.TaskConfig.ExecutorConfig == nil {
		j.jobConfig.TaskConfig.ExecutorConfig = aurora.NewExecutorConfig()
	}

	j.jobConfig.TaskConfig.ExecutorConfig.Name = name
	return j
}

// Will be included as part of entire task inside the scheduler that will be serialized.
func (j *AuroraJob) ExecutorData(data string) Job {

	if j.jobConfig.TaskConfig.ExecutorConfig == nil {
		j.jobConfig.TaskConfig.ExecutorConfig = aurora.NewExecutorConfig()
	}

	j.jobConfig.TaskConfig.ExecutorConfig.Data = data
	return j
}

func (j *AuroraJob) CPU(cpus float64) Job {
	*j.resources[CPU].NumCpus = cpus
	return j
}

func (j *AuroraJob) RAM(ram int64) Job {
	*j.resources[RAM].RamMb = ram
	return j
}

func (j *AuroraJob) Disk(disk int64) Job {
	*j.resources[DISK].DiskMb = disk
	return j
}

func (j *AuroraJob) GPU(gpu int64) Job {
	// GPU resource must be set explicitly since the scheduler by default
	// rejects jobs with GPU resources attached to it.
	if _, ok := j.resources[GPU]; !ok {
		j.resources[GPU] = &aurora.Resource{}
		j.JobConfig().GetTaskConfig().Resources = append(j.JobConfig().GetTaskConfig().Resources, j.resources[GPU])
	}

	j.resources[GPU].NumGpus = &gpu
	return j
}

// How many failures to tolerate before giving up.
func (j *AuroraJob) MaxFailure(maxFail int32) Job {
	j.jobConfig.TaskConfig.MaxTaskFailures = maxFail
	return j
}

// How many instances of the job to run
func (j *AuroraJob) InstanceCount(instCount int32) Job {
	j.jobConfig.InstanceCount = instCount
	return j
}

func (j *AuroraJob) CronSchedule(cron string) Job {
	j.jobConfig.CronSchedule = &cron
	return j
}

func (j *AuroraJob) CronCollisionPolicy(policy aurora.CronCollisionPolicy) Job {
	j.jobConfig.CronCollisionPolicy = policy
	return j
}

// How many instances of the job to run
func (j *AuroraJob) GetInstanceCount() int32 {
	return j.jobConfig.InstanceCount
}

// Restart the job's tasks if they fail
func (j *AuroraJob) IsService(isService bool) Job {
	j.jobConfig.TaskConfig.IsService = isService
	return j
}

// Get the current job configurations key to use for some realis calls.
func (j *AuroraJob) JobKey() *aurora.JobKey {
	return j.jobConfig.Key
}

// Get the current job configurations key to use for some realis calls.
func (j *AuroraJob) JobConfig() *aurora.JobConfiguration {
	return j.jobConfig
}

func (j *AuroraJob) TaskConfig() *aurora.TaskConfig {
	return j.jobConfig.TaskConfig
}

// Add a list of URIs with the same extract and cache configuration. Scheduler must have
// --enable_mesos_fetcher flag enabled. Currently there is no duplicate detection.
func (j *AuroraJob) AddURIs(extract bool, cache bool, values ...string) Job {
	for _, value := range values {
		j.jobConfig.TaskConfig.MesosFetcherUris = append(j.jobConfig.TaskConfig.MesosFetcherUris,
			&aurora.MesosFetcherURI{Value: value, Extract: &extract, Cache: &cache})
	}
	return j
}

// Adds a Mesos label to the job. Note that Aurora will add the
// prefix "org.apache.aurora.metadata." to the beginning of each key.
func (j *AuroraJob) AddLabel(key string, value string) Job {
	if _, ok := j.metadata[key]; ok {
		j.metadata[key].Value = value
	} else {
		j.metadata[key] = &aurora.Metadata{Key: key, Value: value}
		j.jobConfig.TaskConfig.Metadata = append(j.jobConfig.TaskConfig.Metadata, j.metadata[key])
	}
	return j
}

// Add a named port to the job configuration  These are random ports as it's
// not currently possible to request specific ports using Aurora.
func (j *AuroraJob) AddNamedPorts(names ...string) Job {
	j.portCount += len(names)
	for _, name := range names {
		j.jobConfig.TaskConfig.Resources = append(j.jobConfig.TaskConfig.Resources, &aurora.Resource{NamedPort: &name})
	}

	return j
}

// Adds a request for a number of ports to the job configuration. The names chosen for these ports
// will be org.apache.aurora.port.X, where X is the current port count for the job configuration
// starting at 0. These are random ports as it's not currently possible to request
// specific ports using Aurora.
func (j *AuroraJob) AddPorts(num int) Job {
	start := j.portCount
	j.portCount += num
	for i := start; i < j.portCount; i++ {
		portName := "org.apache.aurora.port." + strconv.Itoa(i)
		j.jobConfig.TaskConfig.Resources = append(j.jobConfig.TaskConfig.Resources, &aurora.Resource{NamedPort: &portName})
	}

	return j
}

// From Aurora Docs:
// Add a Value constraint
// name - Mesos slave attribute that the constraint is matched against.
// If negated = true , treat this as a 'not' - to avoid specific values.
// Values - list of values we look for in attribute name
func (j *AuroraJob) AddValueConstraint(name string, negated bool, values ...string) Job {
	j.jobConfig.TaskConfig.Constraints = append(j.jobConfig.TaskConfig.Constraints,
		&aurora.Constraint{
			Name: name,
			Constraint: &aurora.TaskConstraint{
				Value: &aurora.ValueConstraint{
					Negated: negated,
					Values:  values,
				},
				Limit: nil,
			},
		})

	return j
}

// From Aurora Docs:
// A constraint that specifies the maximum number of active tasks on a host with
// a matching attribute that may be scheduled simultaneously.
func (j *AuroraJob) AddLimitConstraint(name string, limit int32) Job {
	j.jobConfig.TaskConfig.Constraints = append(j.jobConfig.TaskConfig.Constraints,
		&aurora.Constraint{
			Name: name,
			Constraint: &aurora.TaskConstraint{
				Value: nil,
				Limit: &aurora.LimitConstraint{Limit: limit},
			},
		})

	return j
}

func (j *AuroraJob) AddDedicatedConstraint(role, name string) Job {
	j.AddValueConstraint("dedicated", false, role+"/"+name)

	return j
}

// Set a container to run for the job configuration to run.
func (j *AuroraJob) Container(container Container) Job {
	j.jobConfig.TaskConfig.Container = container.Build()

	return j
}

// Set a partition policy for the job configuration to implement.
func (j *AuroraJob) PartitionPolicy(policy *aurora.PartitionPolicy) Job {
	j.jobConfig.TaskConfig.PartitionPolicy = policy
	return j
}

// Set the Tier for the Job.
func (j *AuroraJob) Tier(tier string) Job {
	j.jobConfig.TaskConfig.Tier = &tier

	return j
}

// Set an SlaPolicy for the Job.
func (j *AuroraJob) SlaPolicy(policy *aurora.SlaPolicy) Job {
	j.jobConfig.TaskConfig.SlaPolicy = policy

	return j
}
