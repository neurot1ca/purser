package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"purser-hook/internal/pilotreport"
)

func pilotReportMain(args []string) int {
	var jobs []string
	pricesPath := ""
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) || args[i+1] == "" {
			_, _ = fmt.Fprintln(os.Stderr, "usage: purser-hook pilot-report --job JOB_ID [--job JOB_ID ...] [--prices PATH]")
			return 2
		}
		switch args[i] {
		case "--job":
			jobs = append(jobs, args[i+1])
		case "--prices":
			if pricesPath != "" {
				_, _ = fmt.Fprintln(os.Stderr, "--prices may be provided only once")
				return 2
			}
			pricesPath = args[i+1]
		default:
			_, _ = fmt.Fprintln(os.Stderr, "usage: purser-hook pilot-report --job JOB_ID [--job JOB_ID ...] [--prices PATH]")
			return 2
		}
		i++
	}
	if len(jobs) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: purser-hook pilot-report --job JOB_ID [--job JOB_ID ...] [--prices PATH]")
		return 2
	}
	var prices map[string]pilotreport.Prices
	if pricesPath != "" {
		data, err := os.ReadFile(pricesPath)
		if err == nil {
			err = json.Unmarshal(data, &prices)
		}
		if err != nil || prices == nil {
			if err == nil {
				err = fmt.Errorf("expected model price object")
			}
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for model, price := range prices {
			for _, value := range []float64{price.Input, price.Output, price.CacheWrite5m, price.CacheWrite1h, price.CacheRead} {
				if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
					_, _ = fmt.Fprintf(os.Stderr, "invalid nonnegative price for model %q\n", model)
					return 1
				}
			}
		}
	}
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		config = filepath.Join(home, ".claude")
	}
	encoder := json.NewEncoder(os.Stdout)
	for _, job := range jobs {
		if err := encoder.Encode(pilotreport.BuildWithPrices(stateRoot(), filepath.Join(config, "projects"), job, prices)); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}
