package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed packages/*.json
var packageFiles embed.FS

type standardPackage struct {
	PackageID   string          `json:"package_id"`
	Description string          `json:"description"`
	Services    []ServiceConfig `json:"services"`
}

func loadPackages() (map[string]standardPackage, error) {
	names, err := fs.Glob(packageFiles, "packages/*.json")
	if err != nil {
		return nil, err
	}
	result := make(map[string]standardPackage, len(names))
	for _, name := range names {
		data, err := packageFiles.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var pkg standardPackage
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("decode embedded package %s: %w", name, err)
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return nil, fmt.Errorf("decode embedded package %s: trailing data", name)
		}
		if !keyRE.MatchString(pkg.PackageID) || strings.TrimSpace(pkg.Description) == "" || len(pkg.Services) == 0 {
			return nil, fmt.Errorf("invalid embedded package %s", name)
		}
		if _, exists := result[pkg.PackageID]; exists {
			return nil, fmt.Errorf("duplicate embedded package %s", pkg.PackageID)
		}
		result[pkg.PackageID] = pkg
	}
	return result, nil
}

func expandPackages(cfg *Config) error {
	available, err := loadPackages()
	if err != nil {
		return err
	}
	seenPackages := make(map[string]bool, len(cfg.Packages))
	seenServices := make(map[string]bool, len(cfg.Services))
	for _, svc := range cfg.Services {
		seenServices[svc.ServiceID] = true
	}
	var expanded []ServiceConfig
	for _, packageID := range cfg.Packages {
		if seenPackages[packageID] {
			return fmt.Errorf("duplicate package %q", packageID)
		}
		seenPackages[packageID] = true
		pkg, ok := available[packageID]
		if !ok {
			return fmt.Errorf("unknown package %q", packageID)
		}
		for _, svc := range pkg.Services {
			if seenServices[svc.ServiceID] {
				return fmt.Errorf("package %q conflicts with service %q", packageID, svc.ServiceID)
			}
			seenServices[svc.ServiceID] = true
			expanded = append(expanded, svc)
		}
	}
	cfg.Services = append(expanded, cfg.Services...)
	return nil
}

func expandCronJobs(cfg *Config) error {
	enabled := false
	for _, packageID := range cfg.Packages {
		if packageID == "linux.cron.v1" {
			enabled = true
			break
		}
	}
	if !enabled {
		if len(cfg.CronJobs) > 0 {
			return fmt.Errorf("cron_jobs require package %q", "linux.cron.v1")
		}
		return nil
	}
	if len(cfg.CronJobs) == 0 {
		return errors.New("package linux.cron.v1 requires at least one cron job")
	}
	if len(cfg.CronJobs) > 32 {
		return errors.New("cron job limit exceeded")
	}
	if cfg.CronStateDirectory == "" {
		cfg.CronStateDirectory = "/var/lib/bob-icu-cron"
	}
	if !filepath.IsAbs(cfg.CronStateDirectory) || filepath.Clean(cfg.CronStateDirectory) == "/" {
		return errors.New("cron_state_directory must be an absolute non-root path")
	}
	serviceIndex := -1
	for index := range cfg.Services {
		if cfg.Services[index].ServiceID == "linux_cron_jobs" {
			serviceIndex = index
			break
		}
	}
	if serviceIndex < 0 {
		return errors.New("package linux.cron.v1 has no linux_cron_jobs service")
	}
	seen := map[string]bool{}
	for index := range cfg.CronJobs {
		job := &cfg.CronJobs[index]
		if !keyRE.MatchString(job.JobID) || strings.TrimSpace(job.DisplayName) == "" || seen[job.JobID] {
			return fmt.Errorf("invalid or duplicate cron job %q", job.JobID)
		}
		seen[job.JobID] = true
		if job.ExpectedIntervalSeconds < 60 || job.ExpectedIntervalSeconds > 2678400 {
			return fmt.Errorf("invalid expected interval for cron job %s", job.JobID)
		}
		if job.GraceSeconds == 0 {
			job.GraceSeconds = job.ExpectedIntervalSeconds / 10
			if job.GraceSeconds < 300 {
				job.GraceSeconds = 300
			}
			if job.GraceSeconds > 3600 {
				job.GraceSeconds = 3600
			}
		}
		if job.GraceSeconds < 1 || job.GraceSeconds > job.ExpectedIntervalSeconds {
			return fmt.Errorf("invalid grace period for cron job %s", job.JobID)
		}
		if job.MaxRuntimeSeconds == 0 {
			job.MaxRuntimeSeconds = job.ExpectedIntervalSeconds
			if job.MaxRuntimeSeconds > 3600 {
				job.MaxRuntimeSeconds = 3600
			}
		}
		if job.MaxRuntimeSeconds < 1 || job.MaxRuntimeSeconds > job.ExpectedIntervalSeconds {
			return fmt.Errorf("invalid maximum runtime for cron job %s", job.JobID)
		}
		if job.PollIntervalSeconds == 0 {
			job.PollIntervalSeconds = 60
		}
		if job.PollIntervalSeconds < 10 || job.PollIntervalSeconds > 3600 {
			return fmt.Errorf("invalid poll interval for cron job %s", job.JobID)
		}
		cfg.Services[serviceIndex].Sensors = append(cfg.Services[serviceIndex].Sensors, SensorConfig{
			SensorID: job.JobID, DisplayName: job.DisplayName, ValueType: "text",
			IntervalSeconds: job.PollIntervalSeconds, TimeoutSeconds: 5,
			Builtin: "linux.cron_job_state",
			BuiltinOptions: map[string]string{
				"job_id":                    job.JobID,
				"state_directory":           filepath.Clean(cfg.CronStateDirectory),
				"expected_interval_seconds": strconv.FormatInt(job.ExpectedIntervalSeconds, 10),
				"grace_seconds":             strconv.FormatInt(job.GraceSeconds, 10),
				"max_runtime_seconds":       strconv.FormatInt(job.MaxRuntimeSeconds, 10),
			},
		})
	}
	return nil
}

func printPackages(w io.Writer) error {
	packages, err := loadPackages()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(packages))
	for id := range packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		pkg := packages[id]
		if _, err := fmt.Fprintf(w, "%s\t%s\n", pkg.PackageID, pkg.Description); err != nil {
			return err
		}
	}
	return nil
}
