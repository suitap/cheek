package cheek

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/adhocore/gronx"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// OnEvent contains specs on what needs to happen after a job event.
type OnEvent struct {
	TriggerJob         []string `yaml:"trigger_job,omitempty" json:"trigger_job,omitempty"`
	NotifyWebhook      []string `yaml:"notify_webhook,omitempty" json:"notify_webhook,omitempty"`
	NotifySlackWebhook []string `yaml:"notify_slack_webhook,omitempty" json:"notify_slack_webhook,omitempty"`
}

// JobSpec holds specifications and metadata of a job.
type JobSpec struct {
	Cron    string            `yaml:"cron,omitempty" json:"cron,omitempty"`
	Command stringArray       `yaml:"command" json:"command"`
	Params  map[string]string `yaml:"params,omitempty" json:"params,omitempty"`

	OnSuccess OnEvent `yaml:"on_success,omitempty" json:"on_success,omitempty"`
	OnError   OnEvent `yaml:"on_error,omitempty" json:"on_error,omitempty"`

	Name             string            `json:"name"`
	Retries          int               `yaml:"retries,omitempty" json:"retries,omitempty"`
	Env              map[string]string `yaml:"env,omitempty"`
	WorkingDirectory string            `yaml:"working_directory,omitempty" json:"working_directory,omitempty"`
	globalSchedule   *Schedule
	Runs             []JobRun `yaml:"runs,omitempty"`

	nextTick time.Time
	log      zerolog.Logger
	cfg      Config
}

// JobRun holds information about a job execution.
type JobRun struct {
	Status      int `json:"status"`
	logBuf      bytes.Buffer
	Log         string            `json:"log"`
	Name        string            `json:"name"`
	TriggeredAt time.Time         `json:"triggered_at"`
	TriggeredBy string            `json:"triggered_by"`
	Triggered   []string          `json:"triggered,omitempty"`
	Duration    time.Duration     `json:"duration,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	jobRef      *JobSpec
}

func (jr *JobRun) flushLogBuffer() {
	jr.Log = jr.logBuf.String()
}

func (j *JobRun) logToDisk() {
	logFn := path.Join(CheekPath(), fmt.Sprintf("%s.job.jsonl", j.Name))
	f, err := os.OpenFile(logFn,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		j.jobRef.log.Warn().Str("job", j.Name).Err(err).Msgf("Can't open job log '%v' for writing", logFn)
		return
	}
	defer f.Close()

	if err := json.NewEncoder(f).Encode(j); err != nil {
		j.jobRef.log.Warn().Str("job", j.Name).Err(err).Msg("Couldn't save job log to disk.")
	}
}

func (j *JobSpec) finalize(jr *JobRun) {
	// flush logbuf to string
	jr.flushLogBuffer()
	// write logs to disk
	jr.logToDisk()
	// launch on_events
	j.OnEvent(jr)
}

func (j *JobSpec) execCommandWithRetry(trigger string, parameters map[string]string) JobRun {
	tries := 0
	var jr JobRun
	const timeOut = 5 * time.Second

	for tries < j.Retries+1 {

		switch {
		case tries == 0:
			jr = j.execCommand(trigger, parameters)
		default:
			jr = j.execCommand(fmt.Sprintf("%s[retry=%v]", trigger, tries), parameters)
		}

		// finalise logging etc
		j.finalize(&jr)

		if jr.Status == 0 {
			break
		}
		j.log.Debug().Str("job", j.Name).Int("exitcode", jr.Status).Msgf("job exited unsuccessfully, launching retry after %v timeout.", timeOut)
		tries++
		time.Sleep(timeOut)

	}
	return jr
}

func (j JobSpec) now() time.Time {
	// defer for if schedule doesn't exist, allows fore easy testing
	if j.globalSchedule != nil {
		return j.globalSchedule.now()
	}
	return time.Now()
}

func (j *JobSpec) execCommand(trigger string, parameters map[string]string) JobRun {
	j.log.Info().Str("job", j.Name).Str("trigger", trigger).Msgf("Job triggered")
	// init status to non-zero until execution says otherwise
	jr := JobRun{Name: j.Name, TriggeredAt: j.now(), TriggeredBy: trigger, Status: -1, jobRef: j}

	suppressLogs := j.cfg.SuppressLogs

	var cmd *exec.Cmd
	switch len(j.Command) {
	case 0:
		err := errors.New("no command specified")
		jr.Log = fmt.Sprintf("Job unable to start: %v", err.Error())
		j.log.Warn().Str("job", j.Name).Str("trigger", trigger).Err(err).Msg(jr.Log)
		if !suppressLogs {
			fmt.Println(err.Error())
		}
		return jr
	case 1:
		cmd = exec.Command(j.Command[0])
	default:
		params := make([]string, 0, len(j.Command)-1)
		for _, param := range j.Command[1:] {
			tmpl, err := template.New("param").Parse(param)
			if err != nil {
				params = append(params, param)
			} else {
				writer := new(strings.Builder)
				err := tmpl.Execute(writer, parameters)
				if err != nil {
					params = append(params, param)
				} else {
					params = append(params, writer.String())
				}
			}
		}

		cmd = exec.Command(j.Command[0], params...)
	}

	// add env vars
	cmd.Env = os.Environ()
	for k, v := range j.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	cmd.Dir = j.WorkingDirectory

	var w io.Writer
	switch j.cfg.SuppressLogs {
	case true:
		w = &jr.logBuf
	default:
		w = io.MultiWriter(os.Stdout, &jr.logBuf)
	}

	// merge stdout and stderr to same writer
	cmd.Stdout = w
	cmd.Stderr = w

	err := cmd.Start()
	if err != nil {
		if !suppressLogs {
			fmt.Println(err.Error())
		}
		j.log.Warn().Str("job", j.Name).Str("trigger", trigger).Int("exitcode", jr.Status).Err(err).Msg("job unable to start")
		// also send this to terminal output
		_, err = w.Write([]byte(fmt.Sprintf("job unable to start: %v", err.Error())))
		if err != nil {
			j.log.Debug().Str("job", j.Name).Err(err).Msg("can't write to log buffer")
		}

		return jr
	}

	if err := cmd.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			jr.Status = exitError.ExitCode()
			j.log.Warn().Str("job", j.Name).Msgf("Exit code %v", exitError.ExitCode())
		}

		return jr
	}

	jr.Duration = time.Since(jr.TriggeredAt)
	jr.Status = 0
	j.log.Debug().Str("job", j.Name).Int("exitcode", jr.Status).Msgf("job exited status: %v", jr.Status)

	return jr
}

func (j *JobSpec) loadRuns() {
	const nRuns int = 10
	logFn := path.Join(CheekPath(), fmt.Sprintf("%s.job.jsonl", j.Name))
	jrs, err := readLastJobRuns(j.log, logFn, nRuns)
	if err != nil {
		j.log.Warn().Str("job", j.Name).Err(err).Msgf("could not load job logs from '%s'", logFn)
	}
	j.Runs = jrs
}

func (j *JobSpec) setNextTick(refTime time.Time, includeRefTime bool) error {
	if j.Cron != "" {
		t, err := gronx.NextTickAfter(j.Cron, refTime, includeRefTime)
		j.nextTick = t
		return err
	}
	return nil
}

func (j *JobSpec) ValidateCron() error {
	if j.Cron != "" {
		gronx := gronx.New()
		if !gronx.IsValid(j.Cron) {
			return fmt.Errorf("cron string for job '%s' not valid", j.Name)
		}
	}
	return nil
}

func (j *JobSpec) OnEvent(jr *JobRun) {
	var jobsToTrigger []string
	var webhooksToCall []string
	var slackWebhooksToCall []string

	switch jr.Status == 0 {
	case true: // after success
		jobsToTrigger = j.OnSuccess.TriggerJob
		webhooksToCall = j.OnSuccess.NotifyWebhook
		slackWebhooksToCall = j.OnSuccess.NotifySlackWebhook
		if j.globalSchedule != nil {
			jobsToTrigger = append(jobsToTrigger, j.globalSchedule.OnSuccess.TriggerJob...)
			webhooksToCall = append(webhooksToCall, j.globalSchedule.OnSuccess.NotifyWebhook...)
			slackWebhooksToCall = append(slackWebhooksToCall, j.globalSchedule.OnSuccess.NotifySlackWebhook...)
		}
	case false: // after error
		jobsToTrigger = j.OnError.TriggerJob
		webhooksToCall = j.OnError.NotifyWebhook
		slackWebhooksToCall = j.OnError.NotifySlackWebhook
		if j.globalSchedule != nil {
			jobsToTrigger = append(jobsToTrigger, j.globalSchedule.OnError.TriggerJob...)
			webhooksToCall = append(webhooksToCall, j.globalSchedule.OnError.NotifyWebhook...)
			slackWebhooksToCall = append(slackWebhooksToCall, j.globalSchedule.OnError.NotifySlackWebhook...)
		}
	}

	var wg sync.WaitGroup

	for _, tn := range jobsToTrigger {
		tj := j.globalSchedule.Jobs[tn]
		j.log.Debug().Str("job", j.Name).Str("on_event", "job_trigger").Msg("triggered by parent job")
		wg.Add(1)
		go func(wg *sync.WaitGroup) {
			defer wg.Done()
			tj.execCommandWithRetry(fmt.Sprintf("job[%s]", j.Name), make(map[string]string))
		}(&wg)
	}

	// trigger webhooks
	for _, wu := range webhooksToCall {
		j.log.Debug().Str("job", j.Name).Str("on_event", "webhook_call").Msg("triggered by parent job")
		wg.Add(1)
		go func(wg *sync.WaitGroup, webhookURL string) {
			defer wg.Done()
			resp_body, err := JobRunWebhookCall(jr, webhookURL, "generic")
			if err != nil {
				j.log.Warn().Str("job", j.Name).Str("on_event", "webhook").Err(err).Msg("webhook notify failed")
			}
			j.log.Debug().Str("job", jr.Name).Str("webhook_call", "response").Str("webhook_url", webhookURL).Msg(string(resp_body))
		}(&wg, wu)
	}

	// trigger slack webhooks - this feels like a lot of duplication
	for _, wu := range slackWebhooksToCall {
		j.log.Debug().Str("job", j.Name).Str("on_event", "slack_webhook_call").Msg("triggered by parent job")
		wg.Add(1)
		go func(wg *sync.WaitGroup, webhookURL string) {
			defer wg.Done()
			resp_body, err := JobRunWebhookCall(jr, webhookURL, "slack")
			if err != nil {
				j.log.Warn().Str("job", j.Name).Str("on_event", "webhook").Err(err).Msg("webhook notify failed")
			}
			j.log.Debug().Str("job", jr.Name).Str("webhook_call", "response").Str("webhook_url", webhookURL).Msg(string(resp_body))
		}(&wg, wu)
	}

	wg.Wait() // this allows to wait for go routines when running just the job exec
}

func (j JobSpec) ToYAML(includeRuns bool) (string, error) {
	if !includeRuns {
		j.Runs = []JobRun{}
	}

	yData, err := yaml.Marshal(j)
	if err != nil {
		return "", err
	}
	return string(yData), nil
}

// RunJob allows to run a specific job
func RunJob(log zerolog.Logger, cfg Config, scheduleFn string, jobName string) (JobRun, error) {
	s, err := loadSchedule(log, cfg, scheduleFn)
	if err != nil {
		fmt.Printf("error loading schedule: %s\n", err)
		os.Exit(1)
	}
	for _, job := range s.Jobs {
		if job.Name == jobName {
			jr := job.execCommand("manual", make(map[string]string))
			job.finalize(&jr)
			return jr, nil
		}
	}

	return JobRun{}, fmt.Errorf("cannot find job %s in schedule %s", jobName, scheduleFn)
}
