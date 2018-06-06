// Copyright © 2016-2018 Genome Research Limited
// Author: Theo Barber-Bany <tb15@sanger.ac.uk>.
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package scheduler

import (
	"fmt"
	"os/user"

	"github.com/VertebrateResequencing/wr/cloud"
	"github.com/VertebrateResequencing/wr/kubernetes/client"
	kubescheduler "github.com/VertebrateResequencing/wr/kubernetes/scheduler"
	"github.com/VertebrateResequencing/wr/queue"
	"github.com/inconshreveable/log15"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	//	"path/filepath"
	"strings"
	"sync"
	"time"
)

// k8s is the implementer of scheduleri.
// it is a wrapper to implement scheduleri by sending requests to the controller

// maxQueueTime(), reserveTimeout(), hostToID(), busy(), schedule()
// are inherited from local
type k8s struct {
	local
	log15.Logger
	config          *ConfigKubernetes
	libclient       *client.Kubernetesp
	callBackChan    chan string
	cbmutex         sync.RWMutex
	badCallBackChan chan *cloud.Server
	reqChan         chan *kubescheduler.Request
	podAliveChan    chan *kubescheduler.PodAlive
	msgCB           MessageCallBack
	badServerCB     BadServerCallBack
}

// ConfigKubernetes holds configuration options required by
// the kubernetes scheduler.

const defaultScriptName = "DefaultPostCreationScript"

// ConfigKubernetes holds the configuration options for the kubernetes
// WR driver
type ConfigKubernetes struct {
	// The image name (Docker Hub) to pull to run WR Runners with.
	// Defaults to 'ubuntu:latest'
	Image string

	// By default, containers in pods run as root,
	// to run as a different user, specify here.
	User string

	// Requested RAM, a pod will default to 64m, and be
	// allocated more up to a limit
	RAM int

	// Requested Disk space, in GB
	// Currently not implemented: Exploiting node ephemeral storage
	Disk int

	// PostCreationScript is the []byte content of a script you want executed
	// after a server is Spawn()ed. (Overridden during Schedule() by a
	// Requirements.Other["cloud_script"] value.)
	PostCreationScript []byte

	// ConfigFiles is a comma separated list of paths to config files that
	// should be copied over to all spawned servers. Absolute paths are copied
	// over to the same absolute path on the new server. To handle a config file
	// that should remain relative to the home directory (and where the spawned
	// server may have a different username and thus home directory path
	// compared to the current server), use the prefix ~/ to signify the home
	// directory. It silently ignores files that don't exist locally.
	ConfigFiles string

	// DNSNameServers specifies any additional DNS Nameservers to use
	// by default kubernetes uses kubedns, and those set at cluster deployment
	// which will be set by a cluster administrator.
	// See https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#pod-s-dns-config
	// for more details
	DNSNameServers []string

	// Please only use bash for now!
	Shell string

	// StateUpdateFrequency is the frequency at which to check spawned servers
	// that are being used to run things, to see if they're still alive.
	// 0 (default) is treated as 1 minute.
	StateUpdateFrequency time.Duration

	// Namespace to initialise clientsets to
	Namespace string

	// TempMountPath defines the path at which to copy the wr binary to
	// in the container. It points to an empty volume shared between the
	// init container and main container and is where copied files are stored.
	// It should always be the same as what's currently running on the manager
	// otherwise the cmd passed to runCmd() will be incorrect.
	// Also defined as $HOME
	TempMountPath string

	// LocalBinaryPath points to where the wr binary will be accessed to copy
	// to each pod. It should be generated by the invoking command.
	LocalBinaryPath string
}

// Set up prerequisites, call Run()
// Create channels to pass requests to the controller.
// Create queue.
func (s *k8s) initialize(config interface{}, logger log15.Logger) error {
	s.config = config.(*ConfigKubernetes)

	s.Logger = logger.New("scheduler", "kubernetes")

	// make queue
	s.queue = queue.New(localPlace)

	// set our functions for use in schedule() and processQueue()
	s.reqCheckFunc = s.reqCheck
	s.canCountFunc = s.canCount
	s.runCmdFunc = s.runCmd
	s.cancelRunCmdFunc = s.cancelRun
	s.stateUpdateFunc = s.stateUpdate
	s.stateUpdateFreq = s.config.StateUpdateFrequency
	if s.stateUpdateFreq == 0 {
		s.stateUpdateFreq = 1 * time.Minute
	}

	// pass through our shell config and logger to our local embed
	s.local.config = &ConfigLocal{Shell: s.config.Shell}
	s.local.Logger = s.Logger

	// Create the default PostCreationScript
	// If the byte stream does not stringify things may go horribly wrong.
	script := string(s.config.PostCreationScript)
	s.libclient.CreateInitScriptConfigMap(defaultScriptName, script)

	// Set up message notifier & request channels
	s.callBackChan = make(chan string, 5)
	s.badCallBackChan = make(chan *cloud.Server, 5)
	s.reqChan = make(chan *kubescheduler.Request)
	s.podAliveChan = make(chan *kubescheduler.PodAlive)

	// Prerequisites to start the controller
	s.libclient = &client.Kubernetesp{}
	kubeClient, restConfig, err := s.libclient.Authenticate() // Authenticate against the cluster.
	if err != nil {
		return err
	}

	// Initialise all internal clients on  the provided namespace
	err = s.libclient.Initialize(kubeClient, s.config.Namespace)
	if err != nil {
		panic(err)
	}

	// Initialise the informer factory
	// Confine all informers to the provided namespace
	kubeInformerFactory := kubeinformers.NewFilteredSharedInformerFactory(kubeClient, time.Second*30, s.config.Namespace, func(listopts *metav1.ListOptions) {
		listopts.IncludeUninitialized = true
		listopts.Watch = true
	})

	// Rewrite config files.
	files := s.rewriteConfigFiles(s.config.ConfigFiles)
	files = append(files, client.FilePair{s.config.LocalBinaryPath, s.config.TempMountPath})

	// Initialise scheduler opts
	opts := kubescheduler.ScheduleOpts{
		Files:        files,
		CbChan:       s.callBackChan,
		ReqChan:      s.reqChan,
		PodAliveChan: s.podAliveChan,
	}

	// Start listening for messages on call back channels
	go s.notifyCallBack(s.callBackChan, s.badCallBackChan)

	// Create the controller
	controller := kubescheduler.NewController(kubeClient, restConfig, s.libclient, kubeInformerFactory, opts)

	stopCh := make(chan struct{})

	go kubeInformerFactory.Start(stopCh)

	// Start the scheduling controller
	go func() {
		if err = controller.Run(2, stopCh); err != nil {
			logger.Error("Error running controller", err.Error())
		}
	}()

	return nil
}

// Send a request to see if a cmd with the provided requirements
// can ever be scheduled.
// If the request can be scheduled, errChan returns nil then is closed
// If it can't ever be sheduled an error is sent on errChan and returned.
// TODO: OCC if error: What if a node is added shortly after? (Deals with autoscaling?)
// https://godoc.org/k8s.io/apimachinery/pkg/util/wait#ExponentialBackoff
func (s *k8s) reqCheck(req *Requirements) error {
	// Create error channel
	errChan := make(chan error)
	// Rewrite *Requirements to a kubescheduler.Request
	cores := resource.NewMilliQuantity(int64(req.Cores)*1000, resource.DecimalSI)
	ram := resource.NewQuantity(int64(req.RAM)*1024*1024*1024, resource.BinarySI)
	disk := resource.NewQuantity(int64(req.Disk)*1000*1000*1000, resource.DecimalSI)
	r := &kubescheduler.Request{
		RAM:    ram,
		Time:   req.Time,
		Cores:  cores,
		Disk:   disk,
		Other:  req.Other,
		CbChan: errChan,
	}
	// Do i want this to be non blocking??
	// Do i want it to block in a goroutine??

	// Blocking sends are fine in a goroutine?
	go func() {
		s.reqChan <- r
	}()
	// select {
	// case s.reqChan <- r:
	// 	fmt.Println("Request sent")
	// default:
	// 	fmt.Println("No request sent")
	// }
	// Do i want this to block or not?
	// What about multiple errors?
	err := <-errChan

	return err
}

// setMessageCallBack sets the given callback function.
func (s *k8s) setMessageCallback(cb MessageCallBack) {
	s.cbmutex.Lock()
	defer s.cbmutex.Unlock()
	s.msgCB = cb
}

// setBadServerCallBack sets the given callback function.
func (s *k8s) setBadServerCallBack(cb BadServerCallBack) {
	s.cbmutex.Lock()
	defer s.cbmutex.Unlock()
	s.badServerCB = cb
}

// The controller is passed a callback channel.
// notifyMessage recieves on the channel
// if anything is recieved call s.msgCB(msg).
func (s *k8s) notifyCallBack(callBackChan chan string, badCallBackChan chan *cloud.Server) {
	for {
		select {
		case msg := <-callBackChan:
			go s.msgCB(msg)
		case badServer := <-badCallBackChan:
			go s.badServerCB(badServer)
		}
	}

}

// Delete the namespace when all pods have exited.
func (s *k8s) cleanup() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.cleaned = true
	err := s.queue.Destroy()
	if err != nil {
		s.Warn("cleanup queue destruction failed", "err", err)
	}

	err = s.libclient.TearDown()
	if err != nil {
		s.Warn("namespace deletion errored", "err", err)
	}
	return
}

// Work out how many pods with given resource requests can be scheduled based on resource requests on the
// nodes in the cluster.
func (s *k8s) canCount(req *Requirements) (canCount int) {
	// return 1 until I decide what to do.
	return 1
}

// RunFunc calls spawn() and exits with an error = nil when pod has terminated. (Runner exited)
// Or an error if there was a problem. Use deletefunc in controller to send message?
// (based on some sort of channel communication?)
func (s *k8s) runCmd(cmd string, req *Requirements, reservedCh chan bool) error {
	// The first 'argument' to cmd will be the absolute path to the manager's executable.
	// Work out the local binary's name from localBinaryPath.
	//binaryName := filepath.Base(s.config.localBinaryPath)
	configMountPath := "/scripts"

	// Split the cmd into []string
	binaryArgs := strings.Fields(cmd)

	// Create requirements struct
	requirements := &client.ResourceRequest{
		Cores: req.Cores,
		Disk:  req.Disk,
		RAM:   req.RAM,
	}

	pod, err := s.libclient.Spawn(s.config.Image,
		s.config.TempMountPath,
		configMountPath+defaultScriptName,
		binaryArgs,
		defaultScriptName,
		configMountPath,
		requirements)

	if err != nil {
		return err
	}

	// We need to know when the pod we've created (the runner) terminates
	// there is a listener in the controller that will notify when a pod passed
	// to it as a request containing a name and channel is deleted. The notification
	// is the channel being closed.

	// Send the request to the listener.
	errChan := make(chan error)
	go func() {
		req := &kubescheduler.PodAlive{
			Pod:     pod,
			ErrChan: errChan,
		}
		s.podAliveChan <- req
	}()

	// Wait for the response, if there is an error
	// e.g CrashBackLoopoff suggesting the post create
	// script is throwing an error, return it here.
	// Don't delete the pod if some error is thrown.
	err = <-errChan
	if err != nil {
		return err
	}

	// Delete terminated pod if no error thrown.
	err = s.libclient.DestroyPod(pod.ObjectMeta.Name)

	return err
}

// rewrite any relative path to replace '~/' with TempMountPath
// returning []client.FilePair to be copied to the runner.
// currently only relative paths are allowed, any path not
// starting '~/' is dropped as everything ultimately needs
// to go into TempMountPath as that's the volume that gets
// preserved across containers.
func (s *k8s) rewriteConfigFiles(configFiles string) []client.FilePair {
	// get a slice of paths.
	split := strings.Split(configFiles, ",")

	// remove the '~/' prefix as tar will
	// create a ~/.. file. We don't want this.
	rewritten := []string{}
	for _, path := range split {
		if strings.HasPrefix(path, "~/") {
			// Trim prefix
			st := strings.TrimPrefix(path, "~/")
			// Add podBinDir as new prefix
			st = s.config.TempMountPath + st
			rewritten = append(rewritten, st)
		} else {
			s.Logger.Warn(fmt.Sprintf("File with path %s is being ignored as it does not have prefix '~/'", path))
		}
	}

	// create []client.FilePair to pass in to the
	// deploy options.

	// Get absolute paths for all paths in removed
	usr, err := user.Current()
	if err != nil {
		s.Logger.Error(fmt.Sprintf("Failed to get user: %s", usr))
	}
	hDir := usr.HomeDir
	filePairs := []client.FilePair{}
	for i, path := range split {
		if strings.HasPrefix(path, "~/") {
			// // evaluate any symlinks
			// evs, err := filepath.EvalSymlinks(path)
			// if err != nil {
			// 	s.Logger.Error(fmt.Sprintf("Failed to evaluate symlinks for file with path: %s", path))
			// }
			// rewrite ~/ to hDir
			st := strings.TrimPrefix(path, "~/")
			st = hDir + "/" + st

			filePairs = append(filePairs, client.FilePair{st, rewritten[i]})
		}
	}
	return filePairs

}
