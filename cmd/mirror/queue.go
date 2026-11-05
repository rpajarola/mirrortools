package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	iconfig "github.com/rpajarola/mirrortools/internal/config"
	"github.com/rpajarola/mirrortools/internal/daemon"
)

func queueUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: mirror queue <subcommand> [arguments]\n\n")
	fmt.Fprintf(w, "subcommands:\n")
	fmt.Fprintf(w, "  list [-state <s1,s2>] [-site <site>] [-prefix <prefix>] [-limit <n>]  list mirror jobs\n")
	fmt.Fprintf(w, "  submit [flags] <source> [<destdir>] enqueue a mirror job in the daemon\n")
	fmt.Fprintf(w, "  status <id>                         show details for a job\n")
	fmt.Fprintf(w, "  logs [-f] <id>                      show or stream logs for a job\n")
	fmt.Fprintf(w, "  pause [<id>|<prefix>]               pause the queue, an individual job, or site/subtree\n")
	fmt.Fprintf(w, "  resume [<id>|<prefix>]              resume queue processing, an individual job, or site/subtree\n")
	fmt.Fprintf(w, "  concurrency <n>                     change worker concurrency\n")
	fmt.Fprintf(w, "  cancel <id>|<prefix>                cancel a running or queued job, or site/subtree\n")
	fmt.Fprintf(w, "  retry <id>                          retry a failed or quarantined job\n")
	fmt.Fprintf(w, "  quarantine <id>|<prefix> [reason]   quarantine a job or an entire site/subtree\n")
	fmt.Fprintf(w, "  unquarantine <id>|<prefix>          return quarantined job(s) to the queue\n")
	fmt.Fprintf(w, "  delete <id>                         remove a finished or quarantined job\n")
}

func clientFromConfig(cfg *config) *daemon.Client {
	socket := cfg.Daemon.Socket
	if socket == "" {
		socket = iconfig.DefaultSocketPath()
	}
	return daemon.NewUnixClient(socket)
}

func handleClientError(err error, cfg *config) {
	if err == nil {
		return
	}
	socket := cfg.Daemon.Socket
	if socket == "" {
		socket = iconfig.DefaultSocketPath()
	}
	if strings.Contains(err.Error(), "connect: connection refused") || strings.Contains(err.Error(), "no such file or directory") {
		fmt.Fprintf(os.Stderr, "mirror: cannot connect to mirrord at %s\n", socket)
		fmt.Fprintf(os.Stderr, "is the daemon running? Start it with `mirrord` or `systemctl --user start mirrord`\n")
	} else {
		fmt.Fprintf(os.Stderr, "mirror: %v\n", err)
	}
	os.Exit(1)
}

func queueMain(args []string, cfg *config) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		queueUsage(os.Stdout)
		return
	}

	sub := args[0]
	subArgs := args[1:]
	client := clientFromConfig(cfg)
	ctx := context.Background()

	switch sub {
	case "list", "ls":
		fs := flag.NewFlagSet("queue list", flag.ExitOnError)
		stateFlag := fs.String("state", "", "filter by comma-separated states (queued, running, paused, completed, failed, quarantined, cancelled)")
		siteFlag := fs.String("site", "", "filter by site/domain (e.g. example.com)")
		prefixFlag := fs.String("prefix", "", "filter by URL or site prefix")
		limitFlag := fs.Int("limit", 50, "maximum number of jobs to list")
		_ = fs.Parse(subArgs)

		var filter daemon.JobFilter
		if *stateFlag != "" {
			for _, s := range strings.Split(*stateFlag, ",") {
				filter.States = append(filter.States, daemon.State(strings.TrimSpace(s)))
			}
		}
		filter.Site = *siteFlag
		filter.Prefix = *prefixFlag
		filter.Limit = *limitFlag

		jobs, err := client.ListJobs(ctx, filter)
		if err != nil {
			handleClientError(err, cfg)
		}

		status, _ := client.GetStatus(ctx)
		if status != nil {
			pausedStr := ""
			if status.Paused {
				pausedStr = " [PAUSED]"
			}
			fmt.Printf("Queue%s (concurrency: %d) — %d queued, %d running, %d quarantined, %d failed\n\n",
				pausedStr, status.Concurrency, status.Counts[daemon.StateQueued],
				status.Counts[daemon.StateRunning], status.Counts[daemon.StateQuarantined], status.Counts[daemon.StateFailed])
		}

		if len(jobs) == 0 {
			fmt.Println("No jobs found.")
			return
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATE\tMETHOD\tSOURCE\tCREATED\tINFO")
		for _, j := range jobs {
			info := ""
			if j.State == daemon.StateRunning && j.StartedAt != nil {
				info = "running " + time.Since(*j.StartedAt).Round(time.Second).String()
			} else if j.State == daemon.StateFailed && j.LastError != "" {
				info = truncate(j.LastError, 40)
			} else if j.State == daemon.StateQuarantined && j.QuarantineReason != "" {
				info = truncate(j.QuarantineReason, 40)
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
				j.ID, j.State, j.Method, truncate(j.Source, 45),
				time.Since(j.CreatedAt).Round(time.Second).String()+" ago", info)
		}
		w.Flush()

	case "submit":
		submitMain(subArgs, cfg)

	case "status":
		if len(subArgs) < 1 {
			fmt.Fprintf(os.Stderr, "usage: mirror queue status <id>\n")
			os.Exit(2)
		}
		id, err := strconv.ParseInt(subArgs[0], 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mirror: invalid job id %q\n", subArgs[0])
			os.Exit(2)
		}
		j, err := client.GetJob(ctx, id)
		if err != nil {
			handleClientError(err, cfg)
		}
		fmt.Printf("Job #%d:\n", j.ID)
		fmt.Printf("  State:       %s\n", j.State)
		fmt.Printf("  Method:      %s\n", j.Method)
		fmt.Printf("  Source:      %s\n", j.Source)
		fmt.Printf("  Destination: %s\n", j.DestDir)
		if len(j.Flags) > 0 {
			fmt.Printf("  Flags:       %v\n", j.Flags)
		}
		fmt.Printf("  Priority:    %d\n", j.Priority)
		fmt.Printf("  Created:     %s\n", j.CreatedAt.Format(time.RFC3339))
		if j.StartedAt != nil {
			fmt.Printf("  Started:     %s\n", j.StartedAt.Format(time.RFC3339))
		}
		if j.FinishedAt != nil {
			fmt.Printf("  Finished:    %s\n", j.FinishedAt.Format(time.RFC3339))
		}
		if j.RetryCount > 0 {
			fmt.Printf("  Retries:     %d / %d\n", j.RetryCount, j.MaxRetries)
		}
		if j.LastError != "" {
			fmt.Printf("  Last Error:  %s\n", j.LastError)
		}
		if j.QuarantineReason != "" {
			fmt.Printf("  Quarantine:  %s\n", j.QuarantineReason)
		}

	case "logs":
		fs := flag.NewFlagSet("queue logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow/stream live logs")
		_ = fs.Parse(subArgs)
		if fs.NArg() < 1 {
			fmt.Fprintf(os.Stderr, "usage: mirror queue logs [-f] <id>\n")
			os.Exit(2)
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mirror: invalid job id %q\n", fs.Arg(0))
			os.Exit(2)
		}

		if *follow {
			err = client.StreamLogs(ctx, id, func(l *daemon.JobLog) {
				fmt.Printf("[%s] [%s] %s\n", l.Timestamp.Format("15:04:05"), l.Stream, l.Message)
			})
			if err != nil {
				handleClientError(err, cfg)
			}
		} else {
			logs, err := client.GetLogs(ctx, id, 500, 0)
			if err != nil {
				handleClientError(err, cfg)
			}
			for _, l := range logs {
				fmt.Printf("[%s] [%s] %s\n", l.Timestamp.Format("15:04:05"), l.Stream, l.Message)
			}
		}

	case "pause":
		if len(subArgs) == 0 {
			if err := client.PauseQueue(ctx); err != nil {
				handleClientError(err, cfg)
			}
			fmt.Println("Queue paused. Running jobs will finish, but no new jobs will start.")
		} else if id, err := strconv.ParseInt(subArgs[0], 10, 64); err == nil {
			if err := client.PauseJob(ctx, id); err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Job #%d paused.\n", id)
		} else {
			res, err := client.BatchJobs(ctx, &daemon.BatchRequest{Action: daemon.BatchPause, Prefix: subArgs[0]})
			if err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Paused %d jobs matching %s.\n", res.Affected, subArgs[0])
		}

	case "resume":
		if len(subArgs) == 0 {
			if err := client.ResumeQueue(ctx); err != nil {
				handleClientError(err, cfg)
			}
			fmt.Println("Queue resumed.")
		} else if id, err := strconv.ParseInt(subArgs[0], 10, 64); err == nil {
			if err := client.ResumeJob(ctx, id); err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Job #%d resumed.\n", id)
		} else {
			res, err := client.BatchJobs(ctx, &daemon.BatchRequest{Action: daemon.BatchResume, Prefix: subArgs[0]})
			if err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Resumed %d jobs matching %s.\n", res.Affected, subArgs[0])
		}

	case "concurrency":
		if len(subArgs) < 1 {
			fmt.Fprintf(os.Stderr, "usage: mirror queue concurrency <n>\n")
			os.Exit(2)
		}
		n, err := strconv.Atoi(subArgs[0])
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "mirror: concurrency must be a positive integer\n")
			os.Exit(2)
		}
		if err := client.SetConcurrency(ctx, n); err != nil {
			handleClientError(err, cfg)
		}
		fmt.Printf("Worker concurrency set to %d.\n", n)

	case "cancel":
		if len(subArgs) < 1 {
			fmt.Fprintf(os.Stderr, "usage: mirror queue cancel <id>|<prefix>\n")
			os.Exit(2)
		}
		if id, err := strconv.ParseInt(subArgs[0], 10, 64); err == nil {
			if err := client.CancelJob(ctx, id); err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Job #%d cancelled.\n", id)
		} else {
			res, err := client.BatchJobs(ctx, &daemon.BatchRequest{Action: daemon.BatchCancel, Prefix: subArgs[0]})
			if err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Cancelled %d jobs matching %s.\n", res.Affected, subArgs[0])
		}

	case "retry":
		id := requireID("retry", subArgs)
		j, err := client.RetryJob(ctx, id)
		if err != nil {
			handleClientError(err, cfg)
		}
		fmt.Printf("Job #%d moved back to queue.\n", j.ID)

	case "quarantine":
		if len(subArgs) < 1 {
			fmt.Fprintf(os.Stderr, "usage: mirror queue quarantine <id>|<prefix> [reason]\n")
			os.Exit(2)
		}
		reason := "quarantined via CLI"
		if len(subArgs) > 1 {
			reason = strings.Join(subArgs[1:], " ")
		}
		if id, err := strconv.ParseInt(subArgs[0], 10, 64); err == nil {
			if err := client.QuarantineJob(ctx, id, reason); err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Job #%d quarantined.\n", id)
		} else {
			res, err := client.BatchJobs(ctx, &daemon.BatchRequest{Action: daemon.BatchQuarantine, Prefix: subArgs[0], Reason: reason})
			if err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Quarantined %d jobs matching %s.\n", res.Affected, subArgs[0])
		}

	case "unquarantine":
		if len(subArgs) < 1 {
			fmt.Fprintf(os.Stderr, "usage: mirror queue unquarantine <id>|<prefix>\n")
			os.Exit(2)
		}
		if id, err := strconv.ParseInt(subArgs[0], 10, 64); err == nil {
			if err := client.UnquarantineJob(ctx, id); err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Job #%d unquarantined and returned to queue.\n", id)
		} else {
			res, err := client.BatchJobs(ctx, &daemon.BatchRequest{Action: daemon.BatchUnquarantine, Prefix: subArgs[0]})
			if err != nil {
				handleClientError(err, cfg)
			}
			fmt.Printf("Unquarantined %d jobs matching %s.\n", res.Affected, subArgs[0])
		}

	case "delete":
		id := requireID("delete", subArgs)
		if err := client.DeleteJob(ctx, id); err != nil {
			handleClientError(err, cfg)
		}
		fmt.Printf("Job #%d deleted.\n", id)

	default:
		fmt.Fprintf(os.Stderr, "mirror: unknown queue subcommand %q\n\n", sub)
		queueUsage(os.Stderr)
		os.Exit(2)
	}
}

func submitMain(args []string, cfg *config) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println("usage: mirror queue submit [flags] <source> [<destdir>]")
		return
	}

	// Resolve the arguments using the standard CLI resolver so flags & detection match
	inv, err := resolve(args, cfg, os.Stderr)
	if err != nil {
		os.Exit(2)
	}

	flagsMap := make(map[string]string)
	inv.flags.Visit(func(f *flag.Flag) {
		flagsMap[f.Name] = f.Value.String()
	})

	sub := &daemon.SubmitRequest{
		Method:  inv.method.Name,
		Source:  inv.source,
		DestDir: inv.dest,
		Flags:   flagsMap,
	}

	client := clientFromConfig(cfg)
	job, err := client.SubmitJob(context.Background(), sub)
	if err != nil {
		handleClientError(err, cfg)
	}

	fmt.Printf("Enqueued job #%d (%s: %s into %s)\n", job.ID, job.Method, job.Source, job.DestDir)
}

func requireID(cmd string, args []string) int64 {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: mirror queue %s <id>\n", cmd)
		os.Exit(2)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mirror: invalid job id %q\n", args[0])
		os.Exit(2)
	}
	return id
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
