// +build docker

package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/DataDog/datadog-agent/pkg/util/docker"
	log "github.com/cihub/seelog"
)

func initMetadataProviders() {
	if _, err := docker.GetDockerUtil(); err != nil && err != docker.ErrDockerNotAvailable {
		log.Errorf("unable to initialize docker collection: %s", err)
	}
}

// Handles signals - tells us whether we should exit.
func handleSignals(exit chan bool) {
	sigIn := make(chan os.Signal, 100)
	signal.Notify(sigIn)
	// unix only in all likelihood;  but we don't care.
	for sig := range sigIn {
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			log.Criticalf("Caught signal '%s'; terminating.", sig)
			close(exit)
		case syscall.SIGCHLD:
			// Running docker.GetDockerStat() spins up / kills a new process
			continue
		default:
			log.Warnf("Caught signal %s; continuing/ignoring.", sig)
		}
	}
}
