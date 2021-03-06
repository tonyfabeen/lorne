package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log"
	"os"
	"strconv"

	"github.com/flynn/go-discover/discover"
	"github.com/flynn/lorne/types"
	"github.com/flynn/rpcplus"
	"github.com/flynn/sampi/types"
	"github.com/titanous/go-dockerclient"
)

var state = NewState()
var Docker *docker.Client

func main() {
	go attachServer()
	go rpcServer()
	go allocatePorts()

	id := randomID()
	disc, err := discover.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	if err := disc.Register("flynn-lorne-rpc."+id, "1113", nil); err != nil {
		log.Fatal(err)
	}
	if err := disc.Register("flynn-lorne-attach."+id, "1114", nil); err != nil {
		log.Fatal(err)
	}

	services, err := disc.Services("flynn-sampi")
	if err != nil {
		log.Fatal(err)
	}
	schedulers := services.Online()
	if len(schedulers) == 0 {
		log.Fatal("No sampi instances found")
	}

	scheduler, err := rpcplus.DialHTTP("tcp", schedulers[0].Addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Print("Connected to scheduler")

	Docker, err = docker.NewClient("http://localhost:4243")
	if err != nil {
		log.Fatal(err)
	}

	go streamEvents(Docker)
	go syncScheduler(scheduler)

	host := sampi.Host{
		ID:        id,
		Resources: map[string]sampi.ResourceValue{"memory": sampi.ResourceValue{Value: 1024}},
	}

	jobs := make(chan *sampi.Job)
	scheduler.StreamGo("Scheduler.RegisterHost", host, jobs)
	log.Print("Host registered")
	for job := range jobs {
		log.Printf("%#v", job.Config)
		var hostConfig *docker.HostConfig
		if job.TCPPorts > 0 {
			port := strconv.Itoa(<-portAllocator)
			job.Config.Env = append(job.Config.Env, "PORT="+port)
			job.Config.ExposedPorts = map[string]struct{}{port + "/tcp": struct{}{}}
			hostConfig = &docker.HostConfig{
				PortBindings: map[string][]docker.PortBinding{port + "/tcp": {{HostPort: port}}},
			}
		}
		state.AddJob(job)
		container, err := Docker.CreateContainer(job.Config)
		if err == docker.ErrNoSuchImage {
			err = Docker.PullImage(docker.PullImageOptions{Repository: job.Config.Image}, os.Stdout)
			if err != nil {
				log.Fatal(err)
			}
			container, err = Docker.CreateContainer(job.Config)
		}
		if err != nil {
			log.Fatal(err)
		}
		state.SetContainerID(job.ID, container.ID)
		state.WaitAttach(job.ID)
		if err := Docker.StartContainer(container.ID, hostConfig); err != nil {
			log.Fatal(err)
		}
		state.SetStatusRunning(job.ID)
	}
}

func syncScheduler(client *rpcplus.Client) {
	events := make(chan lorne.Event)
	state.AddListener("all", events)
	for event := range events {
		if event.Event != "stop" {
			continue
		}
		log.Println("remove job", event.JobID)
		if err := client.Call("Scheduler.RemoveJobs", []string{event.JobID}, &struct{}{}); err != nil {
			log.Println("remove job", event.JobID, "error:", err)
			// TODO: try to reconnect?
		}
	}
}

func streamEvents(client *docker.Client) {
	stream, err := client.Events()
	if err != nil {
		log.Fatal(err)
	}
	for event := range stream.Events {
		log.Printf("%#v", event)
		if event.Status != "die" {
			continue
		}
		container, err := client.InspectContainer(event.ID)
		if err != nil {
			log.Println("inspect container", event.ID, "error:", err)
			// TODO: set job status anyway?
			continue
		}
		state.SetStatusDone(event.ID, container.State.ExitCode)
	}
	log.Println("events done", stream.Error)
}

func randomID() string {
	b := make([]byte, 16)
	enc := make([]byte, 24)
	_, err := io.ReadFull(rand.Reader, b)
	if err != nil {
		panic(err) // This shouldn't ever happen, right?
	}
	base64.URLEncoding.Encode(enc, b)
	return string(bytes.TrimRight(enc, "="))
}

var portAllocator = make(chan int)

// TODO: fix this, horribly broken
const startPort = 55000
const endPort = 65535

func allocatePorts() {
	for i := startPort; i < endPort; i++ {
		portAllocator <- i
	}
	// TODO: handle wrap-around
}
