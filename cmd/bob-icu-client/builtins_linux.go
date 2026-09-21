package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/thundersteff/bob-icu-client/internal/cronstate"
	"golang.org/x/sys/unix"
)

func knownBuiltin(name string) bool {
	switch name {
	case "host.uptime_seconds", "host.load1", "host.memory_available_percent", "host.root_disk_used_percent",
		"linux.uptime_seconds", "linux.cpu_used_percent", "linux.load1", "linux.load5", "linux.load15",
		"linux.memory_used_percent", "linux.memory_available_bytes", "linux.swap_used_percent",
		"linux.root_disk_used_percent", "linux.root_disk_free_bytes", "linux.root_inode_used_percent", "linux.root_mount_state",
		"linux.systemd_state", "linux.systemd_failed_units_count", "linux.cron_daemon_state", "linux.time_sync_state", "linux.reboot_required_state",
		"linux.cron_job_state":
		return true
	}
	return false
}

func runBuiltinSensor(ctx context.Context, sensor SensorConfig) (SensorResult, error) {
	if sensor.Builtin == "linux.cron_job_state" {
		options, err := parseCronSensorOptions(sensor.BuiltinOptions)
		if err != nil {
			return SensorResult{}, err
		}
		return cronJobStateAt(options, time.Now().UTC()), nil
	}
	return runBuiltin(ctx, sensor.Builtin)
}

type cronSensorOptions struct {
	jobID, stateDirectory                   string
	expectedInterval, grace, maximumRuntime time.Duration
}

func parseCronSensorOptions(values map[string]string) (cronSensorOptions, error) {
	var result cronSensorOptions
	expectedKeys := map[string]bool{
		"job_id": true, "state_directory": true, "expected_interval_seconds": true,
		"grace_seconds": true, "max_runtime_seconds": true,
	}
	if len(values) != len(expectedKeys) {
		return result, errors.New("cron sensor options are incomplete")
	}
	for key := range values {
		if !expectedKeys[key] {
			return result, fmt.Errorf("unknown cron sensor option %q", key)
		}
	}
	result.jobID = values["job_id"]
	result.stateDirectory = values["state_directory"]
	if _, err := cronstate.Path(result.stateDirectory, result.jobID); err != nil {
		return result, err
	}
	parseDuration := func(key string) (time.Duration, error) {
		seconds, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil || seconds < 1 || seconds > 2678400 {
			return 0, fmt.Errorf("invalid %s", key)
		}
		return time.Duration(seconds) * time.Second, nil
	}
	var err error
	if result.expectedInterval, err = parseDuration("expected_interval_seconds"); err != nil {
		return result, err
	}
	if result.grace, err = parseDuration("grace_seconds"); err != nil {
		return result, err
	}
	if result.maximumRuntime, err = parseDuration("max_runtime_seconds"); err != nil {
		return result, err
	}
	if result.grace > result.expectedInterval || result.maximumRuntime > result.expectedInterval {
		return result, errors.New("cron grace and maximum runtime must not exceed expected interval")
	}
	return result, nil
}

func cronJobStateAt(options cronSensorOptions, now time.Time) SensorResult {
	path, _ := cronstate.Path(options.stateDirectory, options.jobID)
	state, err := cronstate.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return SensorResult{Value: "warning", Message: "Noch kein Lauf protokolliert"}
	}
	if err != nil || state.JobID != options.jobID {
		return SensorResult{Value: "critical", Message: "Cron-Zustandsdatei ist ungültig"}
	}
	started, startedOK := parseCronTimestamp(state.LastStartedAt)
	finished, finishedOK := parseCronTimestamp(state.LastFinishedAt)
	succeeded, successOK := parseCronTimestamp(state.LastSuccessAt)
	if state.Running {
		if !startedOK {
			return SensorResult{Value: "critical", Message: "Lauf markiert, aber Startzeit fehlt"}
		}
		age := now.Sub(started)
		if age < 0 {
			return SensorResult{Value: "critical", Message: "Startzeit liegt in der Zukunft"}
		}
		if age > options.maximumRuntime {
			return SensorResult{Value: "critical", Message: fmt.Sprintf("Läuft seit %s; Maximum %s", formatDuration(age), formatDuration(options.maximumRuntime))}
		}
		return SensorResult{Value: "healthy", Message: fmt.Sprintf("Läuft seit %s", formatDuration(age))}
	}
	if state.LastExitCode != nil && *state.LastExitCode != 0 && finishedOK && (!successOK || finished.After(succeeded)) {
		return SensorResult{Value: "critical", Message: fmt.Sprintf("Letzter Lauf fehlgeschlagen (Exit %d)", *state.LastExitCode)}
	}
	if !successOK {
		return SensorResult{Value: "warning", Message: "Noch kein erfolgreicher Lauf protokolliert"}
	}
	age := now.Sub(succeeded)
	if age < 0 {
		return SensorResult{Value: "critical", Message: "Letzter Erfolg liegt in der Zukunft"}
	}
	deadline := options.expectedInterval + options.grace
	if age > deadline {
		return SensorResult{Value: "critical", Message: fmt.Sprintf("Letzter Erfolg vor %s; erwartet innerhalb %s", formatDuration(age), formatDuration(deadline))}
	}
	message := fmt.Sprintf("Letzter Erfolg vor %s", formatDuration(age))
	if state.LastDurationMS != nil {
		message += fmt.Sprintf(" · Laufzeit %s", formatDuration(time.Duration(*state.LastDurationMS)*time.Millisecond))
	}
	return SensorResult{Value: "healthy", Message: message}
}

func parseCronTimestamp(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func formatDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	if value >= 24*time.Hour {
		return fmt.Sprintf("%d Tagen %d Std", int(value/(24*time.Hour)), int((value%(24*time.Hour))/time.Hour))
	}
	if value >= time.Hour {
		return fmt.Sprintf("%d Std %d Min", int(value/time.Hour), int(value%time.Hour/time.Minute))
	}
	if value >= time.Minute {
		return fmt.Sprintf("%d Min %d Sek", int(value/time.Minute), int(value%time.Minute/time.Second))
	}
	return fmt.Sprintf("%d Sek", int(value/time.Second))
}

func runBuiltin(ctx context.Context, name string) (SensorResult, error) {
	switch name {
	case "host.uptime_seconds", "linux.uptime_seconds":
		return uptime()
	case "host.load1", "linux.load1":
		return loadAverage(0)
	case "linux.load5":
		return loadAverage(1)
	case "linux.load15":
		return loadAverage(2)
	case "linux.cpu_used_percent":
		return cpuUsedPercent(ctx)
	case "host.memory_available_percent":
		return memoryMetric("available_percent")
	case "linux.memory_used_percent":
		return memoryMetric("used_percent")
	case "linux.memory_available_bytes":
		return memoryMetric("available_bytes")
	case "linux.swap_used_percent":
		return memoryMetric("swap_used_percent")
	case "host.root_disk_used_percent", "linux.root_disk_used_percent":
		return filesystemMetric("used_percent")
	case "linux.root_disk_free_bytes":
		return filesystemMetric("free_bytes")
	case "linux.root_inode_used_percent":
		return filesystemMetric("inode_used_percent")
	case "linux.root_mount_state":
		return filesystemMetric("mount_state")
	case "linux.systemd_state":
		return systemdState(ctx)
	case "linux.systemd_failed_units_count":
		return systemdFailedUnits(ctx)
	case "linux.cron_daemon_state":
		return cronDaemonState(ctx)
	case "linux.time_sync_state":
		return timeSyncState(ctx)
	case "linux.reboot_required_state":
		return rebootRequiredState(), nil
	default:
		return SensorResult{}, errors.New("unknown builtin")
	}
}

func uptime() (SensorResult, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return SensorResult{}, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return SensorResult{}, errors.New("invalid /proc/uptime")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	return SensorResult{Value: v, Message: "OK"}, err
}

func loadAverage(index int) (SensorResult, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return SensorResult{}, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 || index < 0 || index > 2 {
		return SensorResult{}, errors.New("invalid /proc/loadavg")
	}
	v, err := strconv.ParseFloat(fields[index], 64)
	return SensorResult{Value: v, Message: "OK"}, err
}

type cpuTimes struct{ total, idle uint64 }

func readCPUTimes() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return cpuTimes{}, errors.New("cpu line missing in /proc/stat")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, errors.New("invalid cpu line in /proc/stat")
	}
	var result cpuTimes
	for i, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuTimes{}, err
		}
		result.total += value
		if i == 3 || i == 4 {
			result.idle += value
		}
	}
	return result, scanner.Err()
}

func cpuUsedPercent(ctx context.Context) (SensorResult, error) {
	first, err := readCPUTimes()
	if err != nil {
		return SensorResult{}, err
	}
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return SensorResult{}, ctx.Err()
	case <-timer.C:
	}
	second, err := readCPUTimes()
	if err != nil {
		return SensorResult{}, err
	}
	if second.total < first.total || second.idle < first.idle {
		return SensorResult{}, errors.New("CPU counters moved backwards")
	}
	totalDelta := second.total - first.total
	idleDelta := second.idle - first.idle
	if totalDelta == 0 || idleDelta > totalDelta {
		return SensorResult{}, errors.New("invalid CPU sample")
	}
	return SensorResult{Value: 100 * (1 - float64(idleDelta)/float64(totalDelta)), Message: "OK"}, nil
}

func readMeminfo() (map[string]float64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	values := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		values[strings.TrimSuffix(fields[0], ":")] = value * 1024
	}
	return values, nil
}

func memoryMetric(metric string) (SensorResult, error) {
	values, err := readMeminfo()
	if err != nil {
		return SensorResult{}, err
	}
	total, available := values["MemTotal"], values["MemAvailable"]
	if total <= 0 || available < 0 || available > total {
		return SensorResult{}, errors.New("invalid memory totals")
	}
	switch metric {
	case "available_percent":
		return SensorResult{Value: 100 * available / total, Message: "OK"}, nil
	case "used_percent":
		return SensorResult{Value: 100 * (total - available) / total, Message: "OK"}, nil
	case "available_bytes":
		return SensorResult{Value: available, Message: "OK"}, nil
	case "swap_used_percent":
		swapTotal, swapFree := values["SwapTotal"], values["SwapFree"]
		if swapTotal == 0 {
			return SensorResult{Value: float64(0), Message: "Kein Swap konfiguriert"}, nil
		}
		if swapFree < 0 || swapFree > swapTotal {
			return SensorResult{}, errors.New("invalid swap totals")
		}
		return SensorResult{Value: 100 * (swapTotal - swapFree) / swapTotal, Message: "OK"}, nil
	default:
		return SensorResult{}, errors.New("unknown memory metric")
	}
}

func filesystemMetric(metric string) (SensorResult, error) {
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		return SensorResult{}, err
	}
	switch metric {
	case "used_percent":
		if st.Blocks == 0 {
			return SensorResult{}, errors.New("filesystem block count is zero")
		}
		return SensorResult{Value: 100 * float64(st.Blocks-st.Bavail) / float64(st.Blocks), Message: "OK"}, nil
	case "free_bytes":
		return SensorResult{Value: float64(st.Bavail) * float64(st.Bsize), Message: "OK"}, nil
	case "inode_used_percent":
		if st.Files == 0 {
			return SensorResult{}, errors.New("filesystem inode count is zero")
		}
		return SensorResult{Value: 100 * float64(st.Files-st.Ffree) / float64(st.Files), Message: "OK"}, nil
	case "mount_state":
		readOnly, err := rootMountReadOnly("/proc/1/mountinfo")
		if err != nil {
			return SensorResult{}, err
		}
		if readOnly {
			return SensorResult{Value: "read_only", Message: "Root-Dateisystem ist schreibgeschützt"}, nil
		}
		return SensorResult{Value: "healthy", Message: "Root-Dateisystem ist beschreibbar"}, nil
	default:
		return SensorResult{}, errors.New("unknown filesystem metric")
	}
}

func rootMountReadOnly(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 || fields[4] != "/" {
			continue
		}
		for _, option := range strings.Split(fields[5], ",") {
			switch option {
			case "rw":
				return false, nil
			case "ro":
				return true, nil
			}
		}
		return false, errors.New("root mount has neither rw nor ro option")
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, errors.New("root mount not found in PID 1 mountinfo")
}

func commandOutput(ctx context.Context, command string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func systemdState(ctx context.Context) (SensorResult, error) {
	output, err := commandOutput(ctx, "systemctl", "is-system-running")
	state := strings.TrimSpace(output)
	if state == "running" {
		return SensorResult{Value: "healthy", Message: "systemd: running"}, nil
	}
	if state == "degraded" {
		return SensorResult{Value: "degraded", Message: "systemd: degraded"}, nil
	}
	if state != "" {
		return SensorResult{Value: "critical", Message: "systemd: " + state}, nil
	}
	return SensorResult{}, fmt.Errorf("systemctl is-system-running: %w", err)
}

func systemdFailedUnits(ctx context.Context) (SensorResult, error) {
	output, err := commandOutput(ctx, "systemctl", "--failed", "--no-legend", "--plain", "--no-pager")
	if err != nil {
		return SensorResult{}, fmt.Errorf("systemctl --failed: %w: %s", err, output)
	}
	var units []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			units = append(units, strings.TrimPrefix(fields[0], "●"))
		}
	}
	message := "Keine fehlerhaften Units"
	if len(units) > 0 {
		message = "Fehlerhaft: " + strings.Join(units, ", ")
	}
	return SensorResult{Value: float64(len(units)), Message: message}, nil
}

func cronDaemonState(ctx context.Context) (SensorResult, error) {
	var observations []string
	for _, unit := range []string{"cron.service", "crond.service"} {
		loadState, _ := commandOutput(ctx, "systemctl", "show", unit, "--property=LoadState", "--value")
		if strings.TrimSpace(loadState) != "loaded" {
			continue
		}
		output, err := commandOutput(ctx, "systemctl", "is-active", unit)
		state := strings.TrimSpace(output)
		if err == nil && state == "active" {
			return SensorResult{Value: "healthy", Message: unit + " ist aktiv"}, nil
		}
		if state != "" {
			observations = append(observations, unit+"="+state)
		}
	}
	if len(observations) == 0 {
		return SensorResult{Value: "unavailable", Message: "Kein cron- oder crond-Dienst gefunden"}, nil
	}
	return SensorResult{Value: "critical", Message: strings.Join(observations, ", ")}, nil
}

func timeSyncState(ctx context.Context) (SensorResult, error) {
	output, err := commandOutput(ctx, "timedatectl", "show", "--property=NTPSynchronized", "--value")
	if err != nil {
		return SensorResult{}, fmt.Errorf("timedatectl: %w: %s", err, output)
	}
	if strings.EqualFold(strings.TrimSpace(output), "yes") {
		return SensorResult{Value: "healthy", Message: "NTP synchronisiert"}, nil
	}
	return SensorResult{Value: "critical", Message: "NTP nicht synchronisiert"}, nil
}

func rebootRequiredState() SensorResult {
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		return SensorResult{Value: "required", Message: "Neustart erforderlich"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return SensorResult{Value: "unknown", Message: err.Error()}
	}
	return SensorResult{Value: "healthy", Message: "Kein Neustart erforderlich"}
}
