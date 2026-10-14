package main

import "purser-hook/internal/jobs"

func supervisorMain(args []string) int { return jobs.DurableSupervisorMain(args) }
