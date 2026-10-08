package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/daemon"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	c := apiclient.New()

	switch os.Args[1] {
	case "add":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: sftpsync add <config-file>")
			os.Exit(1)
		}
		cmdAdd(c, os.Args[2])
	case "list", "ls":
		cmdList(c)
	case "remove", "rm":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: sftpsync remove <id>")
			os.Exit(1)
		}
		cmdRemove(c, os.Args[2])
	case "status":
		id := ""
		if len(os.Args) >= 3 {
			id = os.Args[2]
		}
		cmdStatus(c, id)
	case "stop":
		cmdStop(c)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: sftpsync <command> [args]

commands:
  add <config-file>   submit a new sync job
  list                list all jobs
  status [<id>]       show job status (all jobs if no id given)
  remove <id>         stop and remove a job
  stop                shut down the daemon`)
}

func cmdAdd(c *apiclient.Client, configPath string) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		fatalf("resolve path: %v", err)
	}
	job, err := c.AddJob(abs)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("added job %s\n", job.ID)
}

func cmdList(c *apiclient.Client) {
	jobs, err := c.ListJobs()
	if err != nil {
		fatalf("cannot reach daemon (is it running?): %v", err)
	}
	if len(jobs) == 0 {
		fmt.Println("no jobs")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPHASE\tCONFIG\tLAST SYNC\tFILES\tBATCH\tERROR")
	for _, j := range jobs {
		lastSync := "never"
		if !j.Status.LastSync.IsZero() {
			lastSync = j.Status.LastSync.Format("2006-01-02 15:04:05")
		}
		errStr := j.Status.LastError
		if errStr == "" {
			errStr = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			j.ID, j.Status.Phase, j.ConfigPath, lastSync, j.Status.FilesTotal,
			formatBatch(j.Status), errStr)
	}
	tw.Flush()
}

func cmdStatus(c *apiclient.Client, id string) {
	jobs, err := c.ListJobs()
	if err != nil {
		fatalf("cannot reach daemon (is it running?): %v", err)
	}
	if id != "" {
		for _, j := range jobs {
			if j.ID == id {
				printJobDetail(j)
				return
			}
		}
		fatalf("job %s not found", id)
		return
	}
	if len(jobs) == 0 {
		fmt.Println("no jobs")
		return
	}
	for i, j := range jobs {
		printJobDetail(j)
		if i < len(jobs)-1 {
			fmt.Println()
		}
	}
}

func cmdRemove(c *apiclient.Client, id string) {
	if err := c.RemoveJob(id); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("removed job %s\n", id)
}

func cmdStop(c *apiclient.Client) {
	if err := c.Shutdown(); err != nil {
		fatalf("cannot reach daemon (is it running?): %v", err)
	}
	fmt.Println("daemon shutting down")
}

func printJobDetail(j daemon.JobResponse) {
	lastSync := "never"
	if !j.Status.LastSync.IsZero() {
		lastSync = j.Status.LastSync.Format("2006-01-02 15:04:05")
	}
	lastSuccessfulSync := "never"
	if !j.Status.LastSuccessfulSync.IsZero() {
		lastSuccessfulSync = j.Status.LastSuccessfulSync.Format("2006-01-02 15:04:05")
	}
	fmt.Printf("id:           %s\n", j.ID)
	fmt.Printf("config:       %s\n", j.ConfigPath)
	fmt.Printf("added:        %s\n", j.AddedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("phase:        %s\n", j.Status.Phase)
	fmt.Printf("last sync:    %s\n", lastSync)
	fmt.Printf("last success: %s\n", lastSuccessfulSync)
	fmt.Printf("files:        %d\n", j.Status.FilesTotal)
	fmt.Printf("batch:        %s\n", formatBatch(j.Status))
	fmt.Printf("bytes:        %s\n", formatByteProgress(j.Status))
	if !j.Status.StartedAt.IsZero() {
		fmt.Printf("started:      %s\n", j.Status.StartedAt.Format("2006-01-02 15:04:05"))
	}
	if j.Status.CurrentFile != "" {
		fmt.Printf("current:      %s\n", j.Status.CurrentFile)
	}
	if j.Status.LastError != "" {
		fmt.Printf("error:        %s\n", j.Status.LastError)
	}
}

func formatBatch(status daemon.StatusResponse) string {
	if status.BatchTotal == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d complete, %d failed, %d remaining",
		status.Completed, status.BatchTotal, status.Failed, status.Remaining)
}

// formatBytes renders a byte count with a binary unit prefix.
func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	value := float64(n)
	i := -1
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

// formatByteProgress renders batch byte progress as a percentage, or "-" when
// the batch size is not yet known. Completed bytes are clamped so a remote file
// that grew mid-transfer cannot report more than 100%.
func formatByteProgress(status daemon.StatusResponse) string {
	if status.BytesTotal <= 0 {
		return "-"
	}
	completed := status.BytesCompleted
	if completed > status.BytesTotal {
		completed = status.BytesTotal
	}
	if completed < 0 {
		completed = 0
	}
	percent := completed * 100 / status.BytesTotal
	return fmt.Sprintf("%s of %s (%d%%)", formatBytes(completed), formatBytes(status.BytesTotal), percent)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
