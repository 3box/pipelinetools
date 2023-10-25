package jobs

import (
	"fmt"
	"os"
	"time"

	"github.com/3box/pipeline-tools/cd/manager"
	"github.com/3box/pipeline-tools/cd/manager/common/job"
)

// Allow up to 15 minutes for smoke tests to run
const smokeTestFailureTime = 15 * time.Minute

const ClusterName = "ceramic-qa-tests"
const FamilyPrefix = "ceramic-qa-tests-smoke--"
const ContainerName = "ceramic-qa-tests-smoke"
const NetworkConfigurationParameter = "/ceramic-qa-tests-smoke/network_configuration"

var _ manager.Job = &smokeTestJob{}

type smokeTestJob struct {
	baseJob
	env string
	d   manager.Deployment
}

func SmokeTestJob(jobState job.JobState, db manager.Database, notifs manager.Notifs, d manager.Deployment) manager.Job {
	return &smokeTestJob{baseJob{jobState, db, notifs}, os.Getenv("ENV"), d}
}

func (s *smokeTestJob) Advance() (job.JobState, error) {
	now := time.Now()
	switch s.state.Stage {
	case job.JobStage_Queued:
		{
			// No preparation needed so advance the job directly to "dequeued".
			//
			// Don't update the timestamp here so that the "dequeued" event remains at the same position on the timeline
			// as the "queued" event.
			return s.advance(job.JobStage_Dequeued, s.state.Ts, nil)
		}
	case job.JobStage_Dequeued:
		{
			if id, err := s.d.LaunchTask(ClusterName, FamilyPrefix+s.env, ContainerName, NetworkConfigurationParameter, nil); err != nil {
				return s.advance(job.JobStage_Failed, now, err)
			} else {
				// Update the job stage and spawned task identifier
				s.state.Params[job.JobParam_Id] = id
				s.state.Params[job.JobParam_Start] = time.Now().UnixNano()
				return s.advance(job.JobStage_Started, now, err)
			}
		}
	case job.JobStage_Started:
		{
			if started, err := s.checkTests(true); err != nil {
				return s.advance(job.JobStage_Failed, now, err)
			} else if started {
				return s.advance(job.JobStage_Waiting, now, nil)
			} else {
				// Return so we come back again to check
				return s.state, nil
			}
		}
	case job.JobStage_Waiting:
		{
			if stopped, err := s.checkTests(false); err != nil {
				return s.advance(job.JobStage_Failed, now, err)
			} else if stopped {
				return s.advance(job.JobStage_Completed, now, nil)
			} else {
				// Return so we come back again to check
				return s.state, nil
			}
		}
	default:
		{
			return s.advance(job.JobStage_Failed, now, fmt.Errorf("smokeTestJob: unexpected state: %s", manager.PrintJob(s.state)))
		}
	}
}

func (s *smokeTestJob) checkTests(expectedToBeRunning bool) (bool, error) {
	if status, err := s.d.CheckTask(ClusterName, "", expectedToBeRunning, false, s.state.Params[job.JobParam_Id].(string)); err != nil {
		return false, err
	} else if status {
		return true, nil
	} else if expectedToBeRunning && job.IsTimedOut(s.state, manager.DefaultWaitTime) { // Tests did not start in time
		return false, manager.Error_StartupTimeout
	} else if !expectedToBeRunning && job.IsTimedOut(s.state, smokeTestFailureTime) { // Tests did not finish in time
		return false, manager.Error_CompletionTimeout
	} else {
		return false, nil
	}
}
