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
	fmt.Fprintln(tw, "ID\tCONFIG\tLAST SYNC\tFILES\tERROR")
	for _, j := range jobs {
		lastSync := "never"
		if !j.Status.LastSync.IsZero() {
			lastSync = j.Status.LastSync.Format("2006-01-02 15:04:05")
		}
		errStr := j.Status.LastError
		if errStr == "" {
			errStr = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			j.ID, j.ConfigPath, lastSync, j.Status.FilesTotal, errStr)
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
	fmt.Printf("id:        %s\n", j.ID)
	fmt.Printf("config:    %s\n", j.ConfigPath)
	fmt.Printf("added:     %s\n", j.AddedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("last sync: %s\n", lastSync)
	fmt.Printf("files:     %d\n", j.Status.FilesTotal)
	if j.Status.LastError != "" {
		fmt.Printf("error:     %s\n", j.Status.LastError)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
