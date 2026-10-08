package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/daemon"
	"github.com/r1chjames/sftp-sync/internal/humanize"
)

// Exit codes. Usage errors are separated from request failures so scripts can
// tell "you called it wrong" from "the daemon said no".
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// jobClient is the slice of the API client the CLI uses, so command dispatch
// can be tested without a daemon.
type jobClient interface {
	AddJob(configPath string) (daemon.JobResponse, error)
	ListJobs() ([]daemon.JobResponse, error)
	RemoveJob(id string) error
	SyncJob(id string) (daemon.JobResponse, error)
	PauseJob(id string) (daemon.JobResponse, error)
	ResumeJob(id string) (daemon.JobResponse, error)
	Shutdown() error
}

const usageText = `usage: sftpsync <command> [args]

commands:
  add <config-file>   submit a new sync job
  list                list all jobs
  status [<id>]       show job status (all jobs if no id given)
  sync <id>           scan and download now
  pause <id>          stop starting new scans and downloads
  resume <id>         clear the paused state and scan immediately
  remove <id>         stop and remove a job
  stop                shut down the daemon

exit codes:
  0 success, 1 request failed, 2 invalid command line
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, apiclient.New()))
}

// run dispatches a command line and returns the process exit code. It is
// separate from main so argument validation and output can be tested without
// calling os.Exit.
func run(args []string, stdout, stderr io.Writer, c jobClient) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	command := args[0]
	hasArg := len(args) > 1

	switch command {
	case "add":
		if !hasArg {
			return usageError(stderr, "add <config-file>")
		}
		abs, err := filepath.Abs(args[1])
		if err != nil {
			return failedf(stderr, "resolve path: %v", err)
		}
		job, err := c.AddJob(abs)
		if err != nil {
			return failedf(stderr, "%s", failureMessage(err))
		}
		fmt.Fprintf(stdout, "added job %s\n", job.ID)
		return exitOK

	case "list", "ls":
		jobs, err := c.ListJobs()
		if err != nil {
			return failedf(stderr, "%s", failureMessage(err))
		}
		printJobList(stdout, jobs)
		return exitOK

	case "status":
		return runStatus(c, args[1:], stdout, stderr)

	case "sync", "pause", "resume":
		if !hasArg {
			return usageError(stderr, command+" <id>")
		}
		job, err := controlJob(c, command, args[1])
		if err != nil {
			return failedf(stderr, "%s", failureMessage(err))
		}
		fmt.Fprintf(stdout, "%s requested for job %s\n", command, job.ID)
		return exitOK

	case "remove", "rm":
		if !hasArg {
			return usageError(stderr, "remove <id>")
		}
		if err := c.RemoveJob(args[1]); err != nil {
			return failedf(stderr, "%s", failureMessage(err))
		}
		fmt.Fprintf(stdout, "removed job %s\n", args[1])
		return exitOK

	case "stop":
		if err := c.Shutdown(); err != nil {
			return failedf(stderr, "%s", failureMessage(err))
		}
		fmt.Fprintln(stdout, "daemon shutting down")
		return exitOK

	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", command)
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
}

func controlJob(c jobClient, action, id string) (daemon.JobResponse, error) {
	switch action {
	case "sync":
		return c.SyncJob(id)
	case "pause":
		return c.PauseJob(id)
	case "resume":
		return c.ResumeJob(id)
	}
	return daemon.JobResponse{}, fmt.Errorf("unsupported command: %s", action)
}

func runStatus(c jobClient, args []string, stdout, stderr io.Writer) int {
	jobs, err := c.ListJobs()
	if err != nil {
		return failedf(stderr, "%s", failureMessage(err))
	}

	if len(args) > 0 {
		id := args[0]
		for _, j := range jobs {
			if j.ID == id {
				printJobDetail(stdout, j)
				return exitOK
			}
		}
		return failedf(stderr, "job %s not found", id)
	}

	if len(jobs) == 0 {
		fmt.Fprintln(stdout, "no jobs")
		return exitOK
	}
	for i, j := range jobs {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		printJobDetail(stdout, j)
	}
	return exitOK
}

func usageError(stderr io.Writer, usage string) int {
	fmt.Fprintf(stderr, "usage: sftpsync %s\n", usage)
	return exitUsage
}

func failedf(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, format+"\n", a...)
	return exitFailure
}

// failureMessage adds context for the two common failure modes: a daemon that
// is not running, and a job that does not exist.
func failureMessage(err error) string {
	var unreachable *apiclient.UnreachableError
	if errors.As(err, &unreachable) {
		return fmt.Sprintf("cannot reach daemon (is it running?): %v", err)
	}
	return err.Error()
}

func printJobList(w io.Writer, jobs []daemon.JobResponse) {
	if len(jobs) == 0 {
		fmt.Fprintln(w, "no jobs")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPHASE\tCONFIG\tLAST SYNC\tFILES\tBATCH\tERROR")
	for _, j := range jobs {
		errStr := j.Status.LastError
		if errStr == "" {
			errStr = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			j.ID, humanize.Phase(j.Status.Phase, j.Status.Paused), j.ConfigPath, formatTime(j.Status.LastSync),
			j.Status.FilesTotal, formatBatch(j.Status), errStr)
	}
	tw.Flush()
}

func printJobDetail(w io.Writer, j daemon.JobResponse) {
	for _, line := range jobDetailLines(j) {
		fmt.Fprintln(w, line)
	}
}

// jobDetailLines renders a job's status as label/value lines. It is pure so the
// formatting can be tested without capturing stdout.
func jobDetailLines(j daemon.JobResponse) []string {
	lines := []string{
		fmt.Sprintf("id:            %s", j.ID),
		fmt.Sprintf("config:        %s", j.ConfigPath),
		fmt.Sprintf("added:         %s", formatTime(j.AddedAt)),
		fmt.Sprintf("phase:         %s", humanize.Phase(j.Status.Phase, j.Status.Paused)),
		fmt.Sprintf("paused:        %s", yesNo(j.Status.Paused)),
		fmt.Sprintf("last sync:     %s", formatTime(j.Status.LastSync)),
		fmt.Sprintf("last success:  %s", formatTime(j.Status.LastSuccessfulSync)),
		fmt.Sprintf("files:         %d", j.Status.FilesTotal),
		fmt.Sprintf("eligible:      %d", j.Status.EligibleFiles),
		fmt.Sprintf("batch:         %s", formatBatch(j.Status)),
		fmt.Sprintf("bytes:         %s", humanize.BytePair(j.Status.BytesCompleted, j.Status.BytesTotal)),
	}

	if !j.Status.StartedAt.IsZero() {
		lines = append(lines, fmt.Sprintf("batch started: %s", formatTime(j.Status.StartedAt)))
	}
	if j.Status.CurrentFile != "" {
		lines = append(lines, fmt.Sprintf("current file:  %s", j.Status.CurrentFile))
		lines = append(lines, fmt.Sprintf("current bytes: %s",
			humanize.BytePair(j.Status.CurrentFileBytesCompleted, j.Status.CurrentFileBytesTotal)))
	}
	if j.Status.LastError != "" {
		lines = append(lines, fmt.Sprintf("error:         %s", j.Status.LastError))
	}
	return lines
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// formatTime renders a timestamp, or "never" when it is unset.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("2006-01-02 15:04:05")
}

func formatBatch(status daemon.StatusResponse) string {
	if status.BatchTotal == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d complete, %d failed, %d remaining",
		status.Completed, status.BatchTotal, status.Failed, status.Remaining)
}
