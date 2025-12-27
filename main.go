// docker-scheduler is a lightweight service that automatically starts and stops
// Docker containers based on cron schedules defined via container labels.
//
// Container Labels:
//   - docker-scheduler.enabled=true  (required to enable scheduling)
//   - docker-scheduler.start=<cron>  (optional: when to start)
//   - docker-scheduler.stop=<cron>   (optional: when to stop)
//
// Cron format: standard 5-field (minute hour day-of-month month day-of-week)
// Example: "0 8 * * 1-5" = 8:00 AM on weekdays
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/moby/moby/client"
	"github.com/robfig/cron/v3"
)

// Label keys for container scheduling configuration.
const (
	LabelEnabled = "docker-scheduler.enabled"
	LabelStart   = "docker-scheduler.start"
	LabelStop    = "docker-scheduler.stop"
)

// Config holds the application configuration.
type Config struct {
	PollingInterval time.Duration
	Timezone        string
	DockerHost      string
}

// Scheduler manages container start/stop schedules.
type Scheduler struct {
	docker *client.Client
	cron   *cron.Cron
	known  map[string]containerSchedule // containerID -> schedule info
}

// containerSchedule tracks the cron entries for a single container.
type containerSchedule struct {
	startCron string
	stopCron  string
	startID   cron.EntryID
	stopID    cron.EntryID
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[docker-scheduler] ")

	config := loadConfig()

	// Initialize Docker client
	dockerClient, err := client.New(client.WithHost(config.DockerHost))
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	log.Printf("Connected to Docker at %s", config.DockerHost)

	// Initialize scheduler
	scheduler := &Scheduler{
		docker: dockerClient,
		cron:   cron.New(),
		known:  make(map[string]containerSchedule),
	}

	// Initial scan and start polling
	scheduler.scan()
	ticker := time.NewTicker(config.PollingInterval)
	go func() {
		for range ticker.C {
			scheduler.scan()
		}
	}()

	// Start cron scheduler
	scheduler.cron.Start()
	log.Printf("Scheduler started (polling every %s)", config.PollingInterval)

	// Wait for shutdown signal
	waitForShutdown()

	// Graceful shutdown
	log.Println("Shutting down...")
	ticker.Stop()
	<-scheduler.cron.Stop().Done()
	log.Println("Shutdown complete")
}

// loadConfig returns the application configuration from environment variables.
func loadConfig() *Config {
	return &Config{
		PollingInterval: getEnvDuration("POLL_INTERVAL", 10*time.Second),
		Timezone:        getEnv("TZ", "UTC"),
		DockerHost:      getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
	}
}

// getEnv returns the environment variable value or a default.
func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

// getEnvDuration parses a duration from environment variable or returns default.
func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return fallback
}

// waitForShutdown blocks until SIGINT or SIGTERM is received.
func waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan
	log.Printf("Received signal: %s", sig)
}

// scan checks all containers for scheduling labels and updates cron jobs.
func (s *Scheduler) scan() {
	containers, err := s.docker.ContainerList(context.Background(), client.ContainerListOptions{All: true})
	if err != nil {
		log.Printf("Failed to list containers: %v", err)
		return
	}

	seen := make(map[string]bool)

	for _, c := range containers.Items {
		// Check if scheduling is enabled for this container
		if c.Labels[LabelEnabled] != "true" {
			continue
		}

		startCron := c.Labels[LabelStart]
		stopCron := c.Labels[LabelStop]

		// Skip containers without any schedule
		if startCron == "" && stopCron == "" {
			continue
		}

		seen[c.ID] = true
		s.updateContainer(c.ID, startCron, stopCron)
	}

	// Remove schedules for containers that no longer exist
	s.cleanupRemoved(seen)
}

// updateContainer adds or updates schedules for a container.
func (s *Scheduler) updateContainer(id, startCron, stopCron string) {
	known, exists := s.known[id]
	shortID := id[:12]

	// Skip if nothing changed
	if exists && known.startCron == startCron && known.stopCron == stopCron {
		return
	}

	// Remove old schedules if updating
	if exists {
		s.removeSchedule(known)
		log.Printf("[%s] Updating schedules", shortID)
	}

	// Add new schedules
	schedule := containerSchedule{
		startCron: startCron,
		stopCron:  stopCron,
	}

	if startCron != "" {
		if id, err := s.addStartJob(id, startCron); err != nil {
			log.Printf("[%s] Invalid start cron '%s': %v", shortID, startCron, err)
		} else {
			schedule.startID = id
			log.Printf("[%s] Scheduled START: %s", shortID, startCron)
		}
	}

	if stopCron != "" {
		if id, err := s.addStopJob(id, stopCron); err != nil {
			log.Printf("[%s] Invalid stop cron '%s': %v", shortID, stopCron, err)
		} else {
			schedule.stopID = id
			log.Printf("[%s] Scheduled STOP: %s", shortID, stopCron)
		}
	}

	s.known[id] = schedule
}

// addStartJob creates a cron job to start a container.
func (s *Scheduler) addStartJob(containerID, cronExpr string) (cron.EntryID, error) {
	return s.cron.AddFunc(cronExpr, func() {
		shortID := containerID[:12]
		log.Printf("[%s] Starting container", shortID)
		if _, err := s.docker.ContainerStart(context.Background(), containerID, client.ContainerStartOptions{}); err != nil {
			log.Printf("[%s] Failed to start: %v", shortID, err)
		}
	})
}

// addStopJob creates a cron job to stop a container.
func (s *Scheduler) addStopJob(containerID, cronExpr string) (cron.EntryID, error) {
	return s.cron.AddFunc(cronExpr, func() {
		shortID := containerID[:12]
		log.Printf("[%s] Stopping container", shortID)
		if _, err := s.docker.ContainerStop(context.Background(), containerID, client.ContainerStopOptions{}); err != nil {
			log.Printf("[%s] Failed to stop: %v", shortID, err)
		}
	})
}

// removeSchedule removes all cron entries for a container schedule.
func (s *Scheduler) removeSchedule(sched containerSchedule) {
	if sched.startID != 0 {
		s.cron.Remove(sched.startID)
	}
	if sched.stopID != 0 {
		s.cron.Remove(sched.stopID)
	}
}

// cleanupRemoved removes schedules for containers that no longer exist.
func (s *Scheduler) cleanupRemoved(seen map[string]bool) {
	for id, sched := range s.known {
		if !seen[id] {
			s.removeSchedule(sched)
			delete(s.known, id)
			log.Printf("[%s] Removed (container gone)", id[:12])
		}
	}
}
