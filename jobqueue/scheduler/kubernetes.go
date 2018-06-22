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
	"os"
	"path/filepath"

	"github.com/VertebrateResequencing/wr/cloud"
	"github.com/VertebrateResequencing/wr/internal"
	"github.com/VertebrateResequencing/wr/kubernetes/client"
	kubescheduler "github.com/VertebrateResequencing/wr/kubernetes/scheduler"
	"github.com/VertebrateResequencing/wr/queue"
	"github.com/inconshreveable/log15"
	"github.com/sb10/l15h"
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
	logger          log15.Logger
}

// ConfigKubernetes holds configuration options required by
// the kubernetes scheduler.

var defaultScriptName = "wr-default"

const kubeSchedulerLog = "kubeSchedulerLog"

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

	// ConfigMap to use in place of PostCreationScript
	ConfigMap string

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

	// Manager Directory to log to
	ManagerDir string
}

// Set up prerequisites, call Run()
// Create channels to pass requests to the controller.
// Create queue.
func (s *k8s) initialize(config interface{}, logger log15.Logger) error {
	s.config = config.(*ConfigKubernetes)

	s.Logger = logger.New("scheduler", "kubernetes")
	kubeLogFile := filepath.Join(s.config.ManagerDir, kubeSchedulerLog)
	fh, err := log15.FileHandler(kubeLogFile, log15.LogfmtFormat())
	if err != nil {
		return fmt.Errorf("wr kubernetes scheduler could not log to %s: %s", kubeLogFile, err)
	}

	l15h.AddHandler(s.Logger, fh)

	s.Logger.Info(fmt.Sprintf("configuration passed: %#v", s.config))

	// make queue
	s.queue = queue.New(localPlace)
	s.running = make(map[string]int)

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

	// Create the default PostCreationScript if no config map passed.
	// If the byte stream does not stringify things may go horribly wrong.
	if len(s.config.ConfigMap) == 0 {
		if string(s.config.PostCreationScript) != "" {
			script := string(s.config.PostCreationScript)
			s.libclient.CreateInitScriptConfigMap(defaultScriptName, script)
		} else {
			s.Logger.Crit("a config map or post creation script must be provided.")
		}
	}

	// Set up message notifier & request channels
	s.callBackChan = make(chan string, 5)
	s.badCallBackChan = make(chan *cloud.Server, 5)
	s.reqChan = make(chan *kubescheduler.Request)
	s.podAliveChan = make(chan *kubescheduler.PodAlive)

	// Prerequisites to start the controller
	s.libclient = &client.Kubernetesp{}
	kubeClient, restConfig, err := s.libclient.Authenticate(s.Logger) // Authenticate against the cluster.
	if err != nil {
		return err
	}

	// Initialise all internal clients on  the provided namespace
	err = s.libclient.Initialize(kubeClient, s.config.Namespace)
	if err != nil {
		s.Logger.Crit(fmt.Sprintf("failed to initialise the internal clients to namespace %s: %s", s.config.Namespace, err))
		panic(err)
	}

	// Initialise the informer factory
	// Confine all informers to the provided namespace
	kubeInformerFactory := kubeinformers.NewFilteredSharedInformerFactory(kubeClient, time.Second*15, s.config.Namespace, func(listopts *metav1.ListOptions) {
		listopts.IncludeUninitialized = true
		//listopts.Watch = true
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
		Logger:       logger,
		ManagerDir:   s.config.ManagerDir,
	}

	// Start listening for messages on call back channels
	go s.notifyCallBack(s.callBackChan, s.badCallBackChan)

	// Create the controller
	controller := kubescheduler.NewController(kubeClient, restConfig, s.libclient, kubeInformerFactory, opts)
	s.Logger.Info(fmt.Sprintf("Controller contents: %+v", controller))
	stopCh := make(chan struct{})

	go kubeInformerFactory.Start(stopCh)

	// Start the scheduling controller
	s.Logger.Info("Starting scheduling controller")
	go func() {
		if err = controller.Run(2, stopCh); err != nil {
			s.Logger.Error("Error running controller", err.Error())
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
	s.Logger.Info(fmt.Sprintf("reqCheck called with requirements %#v", req))

	// Rewrite *Requirements to a kubescheduler.Request
	cores := resource.NewMilliQuantity(int64(req.Cores)*1000, resource.DecimalSI)
	ram := resource.NewQuantity(int64(req.RAM)*1024*1024, resource.BinarySI)
	disk := resource.NewQuantity(int64(req.Disk)*1000*1000*1000, resource.DecimalSI)
	r := &kubescheduler.Request{
		RAM:    *ram,
		Time:   req.Time,
		Cores:  *cores,
		Disk:   *disk,
		Other:  req.Other,
		CbChan: make(chan error),
	}
	// Do i want this to be non blocking??
	// Do i want it to block in a goroutine??

	// Blocking sends are fine in a goroutine?
	s.Logger.Info(fmt.Sprintf("Sending request to listener %#v", r))
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
	s.Logger.Info("Waiting on reqCheck to return")
	err := <-r.CbChan
	if err != nil {
		//s.msgCB(fmt.Sprintf("Requirements check for request %s recieved error: %s", req.Stringify(), err))
		s.Logger.Info(fmt.Sprintf("Requirements check recieved error: %s", err))
	}

	return err
}

// setMessageCallBack sets the given callback function.
func (s *k8s) setMessageCallback(cb MessageCallBack) {
	s.Logger.Info("setMessageCallBack called")
	s.cbmutex.Lock()
	defer s.cbmutex.Unlock()
	s.msgCB = cb
}

// setBadServerCallBack sets the given callback function.
func (s *k8s) setBadServerCallBack(cb BadServerCallBack) {
	s.Logger.Info("setBadServerCallBack called")
	s.cbmutex.Lock()
	defer s.cbmutex.Unlock()
	s.badServerCB = cb
}

// The controller is passed a callback channel.
// notifyMessage recieves on the channel
// if anything is recieved call s.msgCB(msg).
func (s *k8s) notifyCallBack(callBackChan chan string, badCallBackChan chan *cloud.Server) {
	s.Logger.Info("notifyCallBack called")
	for {
		select {
		case msg := <-callBackChan:
			s.Logger.Info("Callback notification", "msg", msg)
			if s.msgCB != nil {
				go s.msgCB(msg)
			}
		case badServer := <-badCallBackChan:
			s.Logger.Info("Bad server callback notification", "msg", badServer)
			if s.badServerCB != nil {
				go s.badServerCB(badServer)
			}
		}
	}

}

// Delete the namespace when all pods have exited.
func (s *k8s) cleanup() {
	s.Logger.Info("cleanup() Called")

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
	s.Logger.Info("canCount Called, returning 1")
	// return 1 until I decide what to do.
	return 1
}

// RunFunc calls spawn() and exits with an error = nil when pod has terminated. (Runner exited)
// Or an error if there was a problem. Use deletefunc in controller to send message?
// (based on some sort of channel communication?)
func (s *k8s) runCmd(cmd string, req *Requirements, reservedCh chan bool) error {
	s.Logger.Info(fmt.Sprintf("RunCmd Called with cmd %s and requirements %#v", cmd, req))
	// The first 'argument' to cmd will be the absolute path to the manager's executable.
	// Work out the local binary's name from localBinaryPath.
	//binaryName := filepath.Base(s.config.localBinaryPath)
	configMountPath := "/scripts"

	// Split the cmd into []string
	//binaryArgs := strings.Fields(cmd)
	// please work, oh hack.
	cmd = strings.Replace(cmd, "'", "", -1)
	binaryArgs := []string{cmd}

	// Create requirements struct
	requirements := &client.ResourceRequest{
		Cores: req.Cores,
		Disk:  req.Disk,
		RAM:   req.RAM,
	}

	if len(s.config.ConfigMap) != 0 {
		defaultScriptName = s.config.ConfigMap
	}

	//DEBUG:
	//binaryArgs = []string{"tail", "-f", "/dev/null"}

	s.Logger.Info(fmt.Sprintf("Spawning pod with requirements %#v", requirements))
	pod, err := s.libclient.Spawn(s.config.Image,
		s.config.TempMountPath,
		configMountPath+"/"+defaultScriptName+".sh",
		binaryArgs,
		defaultScriptName,
		configMountPath,
		requirements)

	if err != nil {
		s.Logger.Error("error spawning runner pod", "err", err)
		//s.msgCB(fmt.Sprintf("Kubernetes: Was unable to spawn a pod for a runner with requirements %s: %s", req.Stringify(), err))
		reservedCh <- false
		return err
	}

	reservedCh <- true
	s.Logger.Info(fmt.Sprintf("Spawn request succeded, pod %s", pod.ObjectMeta.Name))

	// We need to know when the pod we've created (the runner) terminates
	// there is a listener in the controller that will notify when a pod passed
	// to it as a request containing a name and channel is deleted. The notification
	// is the channel being closed.

	// Send the request to the listener.
	s.Logger.Info(fmt.Sprintf("Sending request to the podAliveChan with pod %s", pod.ObjectMeta.Name))
	errChan := make(chan error)
	go func() {
		req := &kubescheduler.PodAlive{
			Pod:     pod,
			ErrChan: errChan,
			Done:    false,
		}
		s.podAliveChan <- req
	}()

	// Wait for the response, if there is an error
	// e.g CrashBackLoopoff suggesting the post create
	// script is throwing an error, return it here.
	// Don't delete the pod if some error is thrown.
	s.Logger.Info(fmt.Sprintf("Waiting on status of pod %s", pod.ObjectMeta.Name))
	err = <-errChan
	if err != nil {
		s.Logger.Error(fmt.Sprintf("error spawning runner, pod name: %s", pod.ObjectMeta.Name), "err", err)
		return err
	}

	s.Logger.Info(fmt.Sprintf("Deleting pod %s", pod.ObjectMeta.Name))
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
	s.Logger.Info("rewriteConfigFiles Called")
	// Get current user's home directory
	// os.user.Current() was failing in a pod.
	// https://github.com/mitchellh/go-homedir ?
	hDir := os.Getenv("HOME")
	filePairs := []client.FilePair{}
	paths := []string{}

	// Get a slice of paths.
	split := strings.Split(configFiles, ",")

	// Loop over all paths in split, if any don't exist
	// silently remove them.
	for _, path := range split {
		localPath := internal.TildaToHome(path)
		_, err := os.Stat(localPath)
		if err != nil {
			continue
		} else {
			paths = append(paths, path)
		}
	}

	// remove the '~/' prefix as tar will
	// create a ~/.. file. We don't want this.
	// replace '~/' with TempMountPath which we define
	// as $HOME in the created pods.
	// Remove the file name, just returning the
	// directory it is in.
	dests := []string{}
	for _, path := range paths {
		if strings.HasPrefix(path, "~/") {
			// Return only the directory the file is in
			dir := filepath.Dir(path)
			// Trim prefix
			dir = strings.TrimPrefix(dir, "~")
			// Add podBinDir as new prefix
			dir = s.config.TempMountPath + dir
			dests = append(dests, dir)
		} else {
			s.Logger.Warn(fmt.Sprintf("File with path %s is being ignored as it does not have prefix '~/'", path))
		}
	}

	// create []client.FilePair to pass in to the
	// deploy options. Replace '~/' with the current
	// user's $HOME
	for i, path := range paths {
		if strings.HasPrefix(path, "~/") {
			// rewrite ~/ to hDir
			st := strings.TrimPrefix(path, "~/")
			st = hDir + "/" + st

			filePairs = append(filePairs, client.FilePair{st, dests[i]})
		}
	}
	return filePairs

}
