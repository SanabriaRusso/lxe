package cri

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/pkg/pools"
	"github.com/lxc/lxd/lxc/config"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxe/lxf"
	"github.com/lxc/lxe/lxf/device"
	"golang.org/x/net/context"
	utilNet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/tools/remotecommand"
	rtApi "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
)

// streamService implements streaming.Runtime.
type streamService struct {
	streaming.Runtime
	runtimeServer       *RuntimeServer // needed by Exec() endpoint
	streamServer        streaming.Server
	streamServerCloseCh chan struct{}
}

// RuntimeServer is the PoC implementation of the CRI RuntimeServer
type RuntimeServer struct {
	rtApi.RuntimeServiceServer
	lxf       *lxf.LXF
	stream    streamService
	lxdConfig *config.Config
	criConfig *LXEConfig
}

// NewRuntimeServer returns a new RuntimeServer backed by LXD
func NewRuntimeServer(
	criConfig *LXEConfig,
	streamServerAddr string, lxf *lxf.LXF) (*RuntimeServer, error) {
	var err error

	runtime := RuntimeServer{
		criConfig: criConfig,
	}

	configPath, err := getLXDConfigPath(criConfig)
	if err != nil {
		return nil, err
	}
	runtime.lxdConfig, err = config.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	runtime.lxf = lxf

	outboundIP, err := utilNet.ChooseHostInterface()
	if err != nil {
		logger.Errorf("could not find suitable host interface: %v", err)
		return nil, err
	}

	// Prepare streaming server
	streamServerConfig := streaming.DefaultConfig
	streamServerConfig.Addr = streamServerAddr
	streamServerConfig.BaseURL = &url.URL{
		Scheme: "http",
		Host:   outboundIP.String() + ":" + criConfig.LXEStreamingPort,
	}
	runtime.stream.runtimeServer = &runtime
	runtime.stream.streamServer, err = streaming.NewServer(streamServerConfig, runtime.stream)
	if err != nil {
		logger.Errorf("unable to create streaming server")
		return nil, err
	}

	runtime.stream.streamServerCloseCh = make(chan struct{})
	go func() {
		defer close(runtime.stream.streamServerCloseCh)
		logger.Infof("Starting streaming server on %v", streamServerConfig.Addr)
		err := runtime.stream.streamServer.Start(true)
		if err != nil {
			panic(fmt.Errorf("error serving execs or portforwards: %v", err))
		}
	}()

	// schedule rescan for missing volumes
	go func() {
		containers, err := lxf.ListContainers()
		if err != nil {
			return
		}
		for _, c := range containers {
			lxf.AddMonitorTask(c, "volumes", 0, true)
		}
	}()

	return &runtime, nil
}

// Version returns the runtime name, runtime version, and runtime API version.
func (s RuntimeServer) Version(ctx context.Context, req *rtApi.VersionRequest) (*rtApi.VersionResponse, error) {
	logger.Debugf("Version triggered: %v", req)
	version := "0.1.0" // kubelet/remote version, must be 0.1.0
	return &rtApi.VersionResponse{
		Version:           version,
		RuntimeName:       Domain,
		RuntimeVersion:    Version,
		RuntimeApiVersion: version,
	}, nil
}

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes must ensure
// the sandbox is in the ready state on success
func (s RuntimeServer) RunPodSandbox(ctx context.Context,
	req *rtApi.RunPodSandboxRequest) (*rtApi.RunPodSandboxResponse, error) {

	logger.Infof("RunPodSandbox called: SandboxName %v in Namespace %v with SandboxUID %v", req.GetConfig().GetMetadata().GetName(),
		req.GetConfig().GetMetadata().GetNamespace(), req.GetConfig().GetMetadata().GetUid())
	logger.Debugf("RunPodSandbox triggered: %v", req)

	var err error
	sb := &lxf.Sandbox{}

	sb.Hostname = req.GetConfig().GetHostname()
	sb.LogDirectory = req.GetConfig().GetLogDirectory()
	meta := req.GetConfig().GetMetadata()
	sb.Metadata = lxf.SandboxMetadata{
		Attempt:   meta.GetAttempt(),
		Name:      meta.GetName(),
		Namespace: meta.GetNamespace(),
		UID:       meta.GetUid(),
	}
	sb.Labels = req.GetConfig().GetLabels()
	sb.Annotations = req.GetConfig().GetAnnotations()
	sb.Config = map[string]string{}
	sb.RawLXCOptions = make([]lxf.RawLXCOption, 0)

	if req.GetConfig().GetDnsConfig() != nil {
		sb.NetworkConfig.Nameservers = req.GetConfig().GetDnsConfig().GetServers()
		sb.NetworkConfig.Searches = req.GetConfig().GetDnsConfig().GetSearches()
	}

	// Find out which network mode should be used
	if strings.ToLower(req.GetConfig().GetLinux().GetSecurityContext().GetNamespaceOptions().GetNetwork().String()) ==
		string(lxf.NetworkHost) {
		// host network explicitly requested
		sb.NetworkConfig.Mode = lxf.NetworkHost
		setRaw(&sb.RawLXCOptions, lxf.CfgRawLXCInclude, s.criConfig.LXEHostnetworkFile)
	} else if sb.Annotations[fieldLXEBridge] != "" {
		// explicit (external managed) bridge requested
		sb.NetworkConfig.Mode = lxf.NetworkBridged
		sb.NetworkConfig.ModeData = map[string]string{
			"bridge": sb.Annotations[fieldLXEBridge],
		}
	} else if s.criConfig.LXENetworkPlugin == NetworkPluginCNI {
		// lxe is configured to manage network with cni
		sb.NetworkConfig.Mode = lxf.NetworkCNI
	} else {
		// default is to use the predefined lxd bridge managed by lxe
		randIP, err := s.lxf.FindFreeIP(LXEBridge)
		if err != nil {
			logger.Errorf("unable to find a free ip: %v", err)
			return nil, err
		}
		sb.NetworkConfig.Mode = lxf.NetworkBridged
		sb.NetworkConfig.ModeData = map[string]string{
			"bridge":            LXEBridge,
			"interface-address": randIP.String(),
		}
	}

	// If HostPort is defined, set forwardings from that port to the container
	// In lxd, we can use proxy devices for that
	// This can be applied to all NetworkModes except HostNetwork
	if sb.NetworkConfig.Mode != lxf.NetworkHost {
		for _, portMap := range req.Config.PortMappings {
			// both HostPort and ContainerPort must be defined, otherwise invalid
			if portMap.GetHostPort() == 0 || portMap.GetContainerPort() == 0 {
				continue
			}
			hostPort := int(portMap.GetHostPort())
			containerPort := int(portMap.GetContainerPort())

			var protocol device.Protocol
			switch portMap.GetProtocol() {
			case rtApi.Protocol_UDP:
				protocol = device.ProtocolUDP
			case rtApi.Protocol_TCP:
				fallthrough
			default:
				protocol = device.ProtocolTCP
			}

			hostIP := portMap.GetHostIp()
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			containerIP := "127.0.0.1"

			sb.Proxies = append(sb.Proxies, device.Proxy{
				Listen: device.ProxyEndpoint{
					Protocol: protocol,
					Address:  hostIP,
					Port:     hostPort,
				},
				Destination: device.ProxyEndpoint{
					Protocol: protocol,
					Address:  containerIP,
					Port:     containerPort,
				},
			})
		}
	}

	// The following fields allow to specify lxc config not directly represented by the PodSpec
	setRaw(&sb.RawLXCOptions, lxf.CfgRawLXCNamespaces, sb.Annotations[fieldLXENamespaces])

	setRaw(&sb.RawLXCOptions, lxf.CfgRawLXCKernelModules, sb.Annotations[fieldLXEKernelModules])

	for _, mountOption := range strings.Split(sb.Annotations[fieldLXERawMounts], ",") {
		setRaw(&sb.RawLXCOptions, lxf.CfgRawLXCMounts, strings.TrimSpace(mountOption))
	}

	sb.SecurityNesting = strings.Split(sb.Annotations[fieldLXENesting], ",")

	// TODO: Refactor...
	if req.Config.Linux != nil {
		err = lxf.SetIfSet(&sb.Config, "user.linux.cgroup_parent", req.Config.Linux.CgroupParent)
		testErrorEmptyAnnotation(err)

		for key, value := range req.Config.Linux.Sysctls {
			sb.Config["user.linux.sysctls."+key] = value
		}
		if req.Config.Linux.SecurityContext != nil {
			privileged := req.Config.Linux.SecurityContext.Privileged
			sb.Config["user.linux.security_context.privileged"] = strconv.FormatBool(privileged)
			sb.Config["security.privileged"] = strconv.FormatBool(privileged)
			if req.Config.Linux.SecurityContext.NamespaceOptions != nil {
				nsi := "user.linux.security_context.namespace_options"
				nso := req.Config.Linux.SecurityContext.NamespaceOptions

				sb.Config[nsi+".ipc"] = nameSpaceOptionToString(nso.Ipc)
				sb.Config[nsi+".network"] = nameSpaceOptionToString(nso.Network)
				sb.Config[nsi+".pid"] = nameSpaceOptionToString(nso.Pid)
			}

			if req.Config.Linux.SecurityContext.ReadonlyRootfs {
				sb.Disks = append(sb.Disks, device.Disk{
					Path:     "/",
					Readonly: true,
					Pool:     "default",
				})
			}

			if req.Config.Linux.SecurityContext.RunAsUser != nil {
				sb.Config["user.linux.security_context.run_as_user"] =
					strconv.FormatInt(req.Config.Linux.SecurityContext.RunAsUser.Value, 10)
			}

			err = lxf.SetIfSet(&sb.Config, "user.linux.security_context.seccomp_profile_path",
				req.Config.Linux.SecurityContext.SeccompProfilePath)
			testErrorEmptyAnnotation(err)

			if req.Config.Linux.SecurityContext.SelinuxOptions != nil {
				sci := "user.linux.security_context.namespace_options"
				sco := req.Config.Linux.SecurityContext.SelinuxOptions
				testErrorEmptyAnnotation(lxf.SetIfSet(&sb.Config, sci+".role", sco.Role))
				testErrorEmptyAnnotation(lxf.SetIfSet(&sb.Config, sci+".level", sco.Level))
				testErrorEmptyAnnotation(lxf.SetIfSet(&sb.Config, sci+".user", sco.User))
				testErrorEmptyAnnotation(lxf.SetIfSet(&sb.Config, sci+".type", sco.Type))
			}
		}
	}

	err = s.lxf.CreateSandbox(sb)
	if err != nil {
		logger.Errorf("failed to create sandbox, %v", err)
		return nil, err
	}

	logger.Infof("RunPodSandbox successful: Created SandboxID %v for SandboxUID %v", sb.ID, req.GetConfig().GetMetadata().GetUid())

	return &rtApi.RunPodSandboxResponse{
		PodSandboxId: sb.ID,
	}, nil
}

// setRaw calls the lxf.SetRaw method and handles logging
func setRaw(s *[]lxf.RawLXCOption, key, value string) {
	err := lxf.SetRaw(s, key, value)
	if err != nil {
		if serr, ok := err.(*lxf.EmptyAnnotationWarning); ok {
			logger.Debugf("empty Annotation for %s", serr.Where)
		} else {
			logger.Errorf("error occured while adding pod.spec.annotation to raw.lxc: %s", err.Error())
		}
	}
}

func testErrorEmptyAnnotation(err error) {
	if err != nil {
		if serr, ok := err.(*lxf.EmptyAnnotationWarning); ok {
			logger.Debugf("empty Annotation for %s", serr.Where)
		} else {
			logger.Errorf("error occurred while adding pod.spec.annotation to raw.lxc: %s", err)
		}
	}
}

// StopPodSandbox stops any running process that is part of the sandbox and
// reclaims network resources (e.g., IP addresses) allocated to the sandbox.
// If there are any running containers in the sandbox, they must be forcibly
// terminated.
// This call is idempotent, and must not return an error if all relevant
// resources have already been reclaimed. kubelet will call StopPodSandbox
// at least once before calling RemovePodSandbox. It will also attempt to
// reclaim resources eagerly, as soon as a sandbox is not needed. Hence,
// multiple StopPodSandbox calls are expected.
func (s RuntimeServer) StopPodSandbox(ctx context.Context, req *rtApi.StopPodSandboxRequest) (*rtApi.StopPodSandboxResponse, error) {
	logger.Infof("StopPodSandbox called: SandboxID %v", req.GetPodSandboxId())
	logger.Debugf("StopPodSandbox triggered: %v", req)

	_, err := s.cleanupSandbox(req.PodSandboxId)
	if err != nil {
		if lxf.IsErrorNotFound(err) {
			return &rtApi.StopPodSandboxResponse{}, nil
		}
		return nil, err
	}

	return &rtApi.StopPodSandboxResponse{}, nil
}

func (s RuntimeServer) cleanupSandbox(name string) (*lxf.Sandbox, error) {
	profile, err := s.lxf.GetSandbox(name)
	if err != nil {
		return nil, err
	}
	for _, cnt := range profile.UsedBy {
		err = s.lxf.StopContainer(cnt, 30)
		if err != nil {
			logger.Errorf("StopPodSandbox: StopContainer(%v): %v", cnt, err)
			return nil, fmt.Errorf("Stopping container %s failed, %v", cnt, err)
		}
	}

	return profile, s.lxf.StopSandbox(name)
}

// RemovePodSandbox removes the sandbox.
// This is pretty much the same as StopPodSandbox but also removes the sandbox and the containers
func (s RuntimeServer) RemovePodSandbox(ctx context.Context, req *rtApi.RemovePodSandboxRequest) (*rtApi.RemovePodSandboxResponse, error) {
	logger.Infof("RemovePodSandbox called: SandboxID %v", req.GetPodSandboxId())
	logger.Debugf("RemovePodSandbox triggered: %v", req)

	profile, err := s.cleanupSandbox(req.PodSandboxId)
	if err != nil {
		if lxf.IsErrorNotFound(err) {
			return &rtApi.RemovePodSandboxResponse{}, nil
		}
		return nil, err
	}

	for _, cnt := range profile.UsedBy {
		err := s.lxf.DeleteContainer(cnt)
		if err != nil {
			return nil, err
		}
	}

	err = s.lxf.DeleteSandbox(req.GetPodSandboxId())
	if err != nil {
		logger.Errorf("RemovePodSandbox: DeleteProfile(%v): %v", req.GetPodSandboxId(), err)
		return nil, err
	}
	return &rtApi.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus returns the status of the PodSandbox. If the PodSandbox is not
// present, returns an error.
func (s RuntimeServer) PodSandboxStatus(ctx context.Context, req *rtApi.PodSandboxStatusRequest) (*rtApi.PodSandboxStatusResponse, error) {
	//logger.Infof("PodSandboxStatus called: SandboxID %v", req.GetPodSandboxId())
	logger.Debugf("PodSandboxStatus triggered: %v", req)

	sb, err := s.lxf.GetSandbox(req.PodSandboxId)
	if err != nil {
		logger.Errorf("PodSandboxStatus: GetProfile(%v):  %v", req, err)
		return nil, err
	}

	response := rtApi.PodSandboxStatusResponse{
		Status: &rtApi.PodSandboxStatus{
			Id: sb.ID,
			Metadata: &rtApi.PodSandboxMetadata{
				Attempt:   sb.Metadata.Attempt,
				Name:      sb.Metadata.Name,
				Namespace: sb.Metadata.Namespace,
				Uid:       sb.Metadata.UID,
			},
			Linux:       &rtApi.LinuxPodSandboxStatus{},
			Labels:      sb.Labels,
			Annotations: sb.Annotations,
			CreatedAt:   sb.CreatedAt.UnixNano(),
			State: rtApi.PodSandboxState(
				rtApi.PodSandboxState_value["SANDBOX_"+strings.ToUpper(sb.State.String())]),
			Network: &rtApi.PodSandboxNetworkStatus{
				Ip: "",
			},
		},
	}

	for k, v := range sb.Config {
		if strings.HasPrefix(k, "user.linux.security_context.namespace_options.") {
			key := strings.TrimPrefix(k, "user.linux.security_context.namespace_options.")
			if response.Status.Linux.Namespaces == nil {
				response.Status.Linux.Namespaces = &rtApi.Namespace{Options: &rtApi.NamespaceOption{}}
			}
			switch key {
			case "ipc":
				response.Status.Linux.Namespaces.Options.Ipc = stringToNamespaceOption(v)
			case "pid":
				response.Status.Linux.Namespaces.Options.Pid = stringToNamespaceOption(v)
			case "network":
				response.Status.Linux.Namespaces.Options.Network = stringToNamespaceOption(v)
			}
		}
	}

	ip, err := s.lxf.GetSandboxIP(sb)
	if err != nil {
		logger.Errorf("could not look up sandbox ip: %v", err)
	}
	if ip != "" {
		response.Status.Network.Ip = ip
	}

	logger.Debugf("PodSandboxStatus responded: %v", response)
	return &response, nil
}

// ListPodSandbox returns a list of PodSandboxes.
func (s RuntimeServer) ListPodSandbox(ctx context.Context,
	req *rtApi.ListPodSandboxRequest) (*rtApi.ListPodSandboxResponse, error) {
	logger.Debugf("ListPodSandbox triggered: %v", req)

	sandboxes, err := s.lxf.ListSandboxes()
	if err != nil {
		logger.Errorf("ListSandbox(%v): %v", req, err)
		return nil, err
	}

	response := rtApi.ListPodSandboxResponse{}
	for _, sb := range sandboxes {

		state := rtApi.PodSandboxState(
			rtApi.PodSandboxState_value["SANDBOX_"+strings.ToUpper(sb.State.String())])

		if req.GetFilter() != nil {
			filter := req.GetFilter()
			if filter.GetId() != "" && filter.GetId() != sb.ID {
				continue
			}
			if filter.GetState() != nil && state != filter.GetState().GetState() {
				continue
			}
			if !CompareFilterMap(sb.Labels, filter.GetLabelSelector()) {
				continue
			}
		}

		pod := rtApi.PodSandbox{
			Id:        sb.ID,
			CreatedAt: sb.CreatedAt.UnixNano(),
			Metadata: &rtApi.PodSandboxMetadata{
				Attempt:   sb.Metadata.Attempt,
				Name:      sb.Metadata.Name,
				Namespace: sb.Metadata.Namespace,
				Uid:       sb.Metadata.UID,
			},
			State:       state,
			Labels:      sb.Labels,
			Annotations: sb.Annotations,
		}
		response.Items = append(response.Items, &pod)
	}
	logger.Debugf("ListPodSandbox responded: %v", response)
	return &response, nil
}

// CreateContainer creates a new container in specified PodSandbox
func (s RuntimeServer) CreateContainer(ctx context.Context,
	req *rtApi.CreateContainerRequest) (*rtApi.CreateContainerResponse, error) {

	logger.Infof("CreateContainer called: ContainerName %v for SandboxID %v", req.GetConfig().GetMetadata().GetName(), req.GetPodSandboxId())
	logger.Debugf("CreateContainer triggered: %v", req)

	sandbox, err := s.lxf.GetSandbox(req.GetPodSandboxId())
	if err != nil {
		return nil, err
	}

	cnt := &lxf.Container{
		Sandbox: sandbox,
	}

	cnt.Labels = req.GetConfig().GetLabels()
	cnt.Annotations = req.GetConfig().GetAnnotations()
	meta := req.GetConfig().GetMetadata()
	cnt.Metadata = lxf.ContainerMetadata{
		Attempt: meta.GetAttempt(),
		Name:    meta.GetName(),
	}
	cnt.LogPath = req.GetConfig().GetLogPath()
	cnt.Image = req.GetConfig().GetImage().GetImage()
	cnt.Config = make(map[string]string)

	// if the container was moved, we need to clear out these fields
	// TODO: make the a mapping of device.* fields by key, so it would override existings
	cnt.Blocks = []device.Block{}
	cnt.Disks = []device.Disk{}
	cnt.Nics = []device.Nic{}
	cnt.Proxies = []device.Proxy{}

	for _, mnt := range req.GetConfig().GetMounts() {
		// resolve host path symlinks
		hostPath, err := filepath.EvalSymlinks(mnt.GetHostPath())
		if err != nil {
			logger.Errorf("CreateContainer: could not eval symlink (%v): %v", req, err)
			return nil, err
		}

		optional := (strings.HasPrefix(mnt.GetContainerPath(), "/var/run/") ||
			(strings.HasPrefix(mnt.GetContainerPath(), "/dev/termination-log")))

		cnt.Disks = append(cnt.Disks, device.Disk{
			Path:     mnt.GetContainerPath(),
			Source:   hostPath,
			Readonly: mnt.GetReadonly(),
			Optional: optional,
		})
	}

	for _, dev := range req.GetConfig().GetDevices() {
		cnt.Blocks = append(cnt.Blocks, device.Block{
			Source: dev.GetHostPath(),
			Path:   dev.GetContainerPath(),
		})
	}

	cnt.Privileged = req.GetConfig().GetLinux().GetSecurityContext().GetPrivileged()

	// get metadata & cloud-init if defined
	otherEnvs := make(map[string]string)
	for _, env := range req.GetConfig().GetEnvs() {
		if env.GetKey() == "user-data" {
			cnt.CloudInitUserData = env.GetValue()
		} else if env.GetKey() == "meta-data" {
			cnt.CloudInitMetaData = env.GetValue()
		} else if env.GetKey() == "network-config" {
			cnt.CloudInitNetworkConfig = env.GetValue()
		} else {
			otherEnvs[env.GetKey()] = env.GetValue()
		}
	}
	cnt.EnvironmentVars = otherEnvs

	// append other envs below metadata
	if cnt.CloudInitMetaData != "" && len(otherEnvs) > 0 {
		cnt.CloudInitMetaData += "\n"
	}

	err = s.lxf.CreateContainer(cnt)
	if err != nil {
		logger.Errorf("failed to create container: %v", err)
		return nil, err
	}

	logger.Infof("CreateContainer successful: Created ContainerID %v for SandboxID %v", cnt.ID, req.GetPodSandboxId())

	return &rtApi.CreateContainerResponse{
		ContainerId: cnt.ID,
	}, err
}

// StartContainer starts the container.
// nolint: dupl
func (s RuntimeServer) StartContainer(ctx context.Context,
	req *rtApi.StartContainerRequest) (*rtApi.StartContainerResponse, error) {
	logger.Infof("StartContainer called: ContainerID %v", req.GetContainerId())
	logger.Debugf("StartContainer triggered: %v", req)

	err := s.lxf.StartContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}

	// Wait until container has a ip address
	// This was an attempt to omit the `Ip: "127.0.0.1"` in the status return, since kubelet is not happy when it takes too
	// long to assign an IP to a sandbox, but this didn't help. kubelet stops it right after container finally obtained one
	// c, err := s.lxf.GetContainer(req.ContainerId)
	// if err != nil {
	// 	return nil, err
	// }
	// tick := time.Tick(500 * time.Millisecond)
	// timeout := time.After(30 * time.Second)
	// for {
	// 	select {
	// 	case <-tick:
	// 		ip, err := s.lxf.GetSandboxIP(c.Sandbox)
	// 		if err != nil {
	// 			return nil, err
	// 		}
	// 		if ip != "" {
	// 			return &rtApi.StartContainerResponse{}, nil
	// 		}
	// 	case <-timeout:
	// 		err := fmt.Errorf("Container %v hasn't got IP within time", req.ContainerId)
	// 		logger.Error(err.Error())
	// 		return nil, err
	// 	}
	// }

	return &rtApi.StartContainerResponse{}, nil
}

// StopContainer stops a running container with a grace period (i.e., timeout).
// This call is idempotent, and must not return an error if the container has
// already been stopped.
// nolint: dupl
func (s RuntimeServer) StopContainer(ctx context.Context,
	req *rtApi.StopContainerRequest) (*rtApi.StopContainerResponse, error) {
	logger.Infof("StopContainer called: ContainerID %v", req.GetContainerId())
	logger.Debugf("StopContainer triggered: %v", req)

	err := s.lxf.StopContainer(req.GetContainerId(), int(req.GetTimeout()))
	if err != nil {
		return nil, err
	}

	return &rtApi.StopContainerResponse{}, nil
}

// RemoveContainer removes the container. If the container is running, the
// container must be forcibly removed.
// This call is idempotent, and must not return an error if the container has
// already been removed.
// nolint: dupl
func (s RuntimeServer) RemoveContainer(ctx context.Context, req *rtApi.RemoveContainerRequest) (*rtApi.RemoveContainerResponse, error) {
	logger.Infof("RemoveContainer called: ContainerID %v", req.GetContainerId())
	logger.Debugf("RemoveContainer triggered: %v", req)

	err := s.lxf.DeleteContainer(req.GetContainerId())
	if err != nil {
		return nil, err
	}

	return &rtApi.RemoveContainerResponse{}, nil
}

// ListContainers lists all containers by filters.
func (s RuntimeServer) ListContainers(ctx context.Context, req *rtApi.ListContainersRequest) (*rtApi.ListContainersResponse, error) {
	logger.Debugf("ListContainers triggered: %v", req)

	var response rtApi.ListContainersResponse
	ctslist, err := s.lxf.ListContainers()
	if err != nil {
		logger.Errorf("Unable to get list of containers: %v", err)
		return nil, err
	}

	for _, cinfo := range ctslist {
		rspCnt := toCriContainer(cinfo)
		rspCnt.Image = &rtApi.ImageSpec{Image: cinfo.Image}
		rspCnt.ImageRef = cinfo.Image

		if req.GetFilter() != nil {
			filter := req.GetFilter()
			if filter.GetId() != "" && filter.GetId() != cinfo.ID {
				continue
			}
			if filter.GetState() != nil && filter.GetState().GetState() != rspCnt.State {
				continue
			}
			if filter.GetPodSandboxId() != "" && filter.GetPodSandboxId() != cinfo.Sandbox.ID {
				continue
			}
			if !CompareFilterMap(rspCnt.Labels, filter.GetLabelSelector()) {
				continue
			}
		}

		response.Containers = append(response.Containers, rspCnt)
	}

	logger.Debugf("ListContainers responded: %v", response)
	return &response, nil
}

// ContainerStatus returns status of the container. If the container is not
// present, returns an error.
func (s RuntimeServer) ContainerStatus(ctx context.Context, req *rtApi.ContainerStatusRequest) (*rtApi.ContainerStatusResponse, error) {
	//logger.Infof("ContainerStatus called: ContainerID %v", req.GetContainerId())
	logger.Debugf("ContainerStatus triggered: %v", req)

	ct, err := s.lxf.GetContainer(req.ContainerId)
	if err != nil {
		logger.Errorf("Unable to GetContainerStatus(%q) containers: %v", req.ContainerId, err)
		return nil, err
	}

	response := toCriStatusResponse(ct)

	logger.Debugf("ContainerStatus responded: %v", response)
	return response, nil
}

// UpdateContainerResources updates ContainerConfig of the container.
func (s RuntimeServer) UpdateContainerResources(ctx context.Context,
	req *rtApi.UpdateContainerResourcesRequest) (*rtApi.UpdateContainerResourcesResponse, error) {

	logger.Debugf("UpdateContainerResources triggered: %v", req)
	return nil, fmt.Errorf("UpdateContainerResources not implemented")
}

// ReopenContainerLog asks runtime to reopen the stdout/stderr log file
// for the container. This is often called after the log file has been
// rotated. If the container is not running, container runtime can choose
// to either create a new log file and return nil, or return an error.
// Once it returns error, new container log file MUST NOT be created.
func (s RuntimeServer) ReopenContainerLog(ctx context.Context, req *rtApi.ReopenContainerLogRequest) (
	*rtApi.ReopenContainerLogResponse, error) {
	logger.Debugf("ReopenContainerLog triggered: %v", req)
	return nil, fmt.Errorf("ReopenContainerLog not implemented")
}

// ExecSync runs a command in a container synchronously.
func (s RuntimeServer) ExecSync(ctx context.Context, req *rtApi.ExecSyncRequest) (*rtApi.ExecSyncResponse, error) {
	logger.Debugf("ExecSync triggered: %v", req)

	out, err := s.lxf.ExecSync(req.ContainerId, req.Cmd)
	if err != nil {
		logger.Errorf("exec-sync '%v' failed to execute on container '%v', %v",
			strings.Join(req.Cmd, " "), req.ContainerId, err)
		return nil, err
	}

	return &rtApi.ExecSyncResponse{
		Stderr:   out.StdErr,
		Stdout:   out.StdOut,
		ExitCode: int32(out.Code),
	}, nil
}

// Exec prepares a streaming endpoint to execute a command in the container.
func (s RuntimeServer) Exec(ctx context.Context, req *rtApi.ExecRequest) (*rtApi.ExecResponse, error) {
	logger.Debugf("Exec triggered: %v", req)

	resp, err := s.stream.streamServer.GetExec(req)
	if err != nil {
		return nil, fmt.Errorf("unable to prepare exec endpoint")
	}

	logger.Debugf("Exec responded: %v", resp)

	return resp, nil
}

func (ss streamService) Exec(containerID string, cmd []string,
	stdin io.Reader, stdout, stderr io.WriteCloser,
	_ bool, resize <-chan remotecommand.TerminalSize) error {

	logger.Debugf("StreamService triggered: {containerID: %v, cmd: %v, stdin: %v, stdout: %v, stderr: %v}",
		containerID, cmd, stdin, stdout, stderr)

	_, err := ss.runtimeServer.lxf.Exec(containerID, cmd, stdin, stdout, stderr)

	if err != nil {
		logger.Errorf("exec container error: %v", err)
		return err
	}

	return nil
}

// Attach prepares a streaming endpoint to attach to a running container.
func (s RuntimeServer) Attach(ctx context.Context, req *rtApi.AttachRequest) (*rtApi.AttachResponse, error) {
	logger.Debugf("Attach triggered: %v", req)
	logger.Errorf("Attach - not implemented")
	return nil, fmt.Errorf("Attach - not implemented")
}

// PortForward prepares a streaming endpoint to forward ports from a PodSandbox.
func (s RuntimeServer) PortForward(ctx context.Context, req *rtApi.PortForwardRequest) (resp *rtApi.PortForwardResponse, err error) {
	logger.Debugf("PortForward triggered: %v", req)

	resp, err = s.stream.streamServer.GetPortForward(req)
	if err != nil {
		return nil, fmt.Errorf("unable to prepare portforward endpoint")
	}

	return resp, nil
}

func (ss streamService) PortForward(podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	pod, err := ss.runtimeServer.PodSandboxStatus(nil, &rtApi.PodSandboxStatusRequest{PodSandboxId: podSandboxID})
	if err != nil {
		err = fmt.Errorf("PortForward: ss.PortForward() PodSandboxStatus(%v): %v", podSandboxID, err)
		logger.Errorf("%v", err)
		return err
	}

	if pod.Status.Network == nil {
		err = fmt.Errorf("PortForward: ss.PortForward() This pod (%v) has no IP", podSandboxID)
		logger.Errorf("%v", err)
		return err
	}
	podIP := pod.Status.Network.Ip

	_, err = exec.LookPath("socat")
	if err != nil {
		err = fmt.Errorf("unable to do port forwarding: socat not found")
		logger.Errorf("%v", err)
		return err
	}

	args := []string{"-", fmt.Sprintf("TCP4:%s:%d,keepalive", podIP, port)}

	commandString := fmt.Sprintf("socat %s", strings.Join(args, " "))
	logger.Debugf("executing port forwarding command: %s", commandString)

	command := exec.Command("socat", args...) // nolint: gosec #nosec
	command.Stdout = stream

	stderr := new(bytes.Buffer)
	command.Stderr = stderr

	// If we use Stdin, command.Run() won't return until the goroutine that's copying
	// from stream finishes. Unfortunately, if you have a client like telnet connected
	// via port forwarding, as long as the user's telnet client is connected to the user's
	// local listener that port forwarding sets up, the telnet session never exits. This
	// means that even if socat has finished running, command.Run() won't ever return
	// (because the client still has the connection and stream open).
	//
	// The work around is to use StdinPipe(), as Wait() (called by Run()) closes the pipe
	// when the command (socat) exits.
	inPipe, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("unable to do port forwarding: error creating stdin pipe: %v", err)
	}
	go func() {
		_, err = pools.Copy(inPipe, stream)
		if err != nil {
			logger.Errorf("pipe copy errored: %v", err)
		}
		err = inPipe.Close()
		if err != nil {
			logger.Errorf("pipe close errored: %v", err)
		}
	}()

	if err := command.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}

	return nil
}

// ContainerStats returns stats of the container. If the container does not
// exist, the call returns an error.
func (s RuntimeServer) ContainerStats(ctx context.Context, req *rtApi.ContainerStatsRequest) (*rtApi.ContainerStatsResponse, error) {
	logger.Debugf("ContainerStats triggered: %v", req)
	response := rtApi.ContainerStatsResponse{}

	cntStat, err := s.lxf.GetContainer(req.ContainerId)
	if err != nil {
		logger.Errorf("ContainerStats: GetContainerState(%v): %v", req.ContainerId, err)
		return nil, err
	}
	response.Stats = toCriStats(cntStat)

	logger.Debugf("ContainerStats responded: %v", response)
	return &response, nil
}

// ListContainerStats returns stats of all running containers.
func (s RuntimeServer) ListContainerStats(ctx context.Context,
	req *rtApi.ListContainerStatsRequest) (*rtApi.ListContainerStatsResponse, error) {

	logger.Debugf("ListContainerStats triggered: %v", req)

	response := rtApi.ListContainerStatsResponse{}

	if req.Filter != nil && req.Filter.Id != "" {
		c, err := s.lxf.GetContainer(req.Filter.Id)
		if err != nil {
			logger.Errorf("ListContainerStats: ContainerStats(%v): %v", req.Filter.Id, err)
			return nil, err
		}
		response.Stats = append(response.Stats, toCriStats(c))
		return &response, nil
	}

	cts, err := s.lxf.ListContainers()
	if err != nil {
		logger.Errorf("ContainerStats: list containers failed, %v", err)
		return nil, err
	}

	for _, ct := range cts {
		response.Stats = append(response.Stats, toCriStats(ct))
	}

	logger.Debugf("ListContainerStats responded: %v", response)
	return &response, nil
}

// UpdateRuntimeConfig updates the runtime configuration based on the given request.
func (s RuntimeServer) UpdateRuntimeConfig(ctx context.Context,
	req *rtApi.UpdateRuntimeConfigRequest) (*rtApi.UpdateRuntimeConfigResponse, error) {
	//logger.Infof("UpdateRuntimeConfig called: PodCIDR %v", req.GetRuntimeConfig().GetNetworkConfig().GetPodCidr())
	logger.Debugf("UpdateRuntimeConfig triggered: %v", req)

	podCIDR := req.GetRuntimeConfig().GetNetworkConfig().GetPodCidr()
	err := s.lxf.EnsureBridge(LXEBridge, podCIDR, true, false)
	if err != nil {
		logger.Errorf("UpdateRuntimeConfig: %v", err)
		return nil, err
	}

	return &rtApi.UpdateRuntimeConfigResponse{}, nil
}

// Status returns the status of the runtime.
func (s RuntimeServer) Status(ctx context.Context, req *rtApi.StatusRequest) (*rtApi.StatusResponse, error) {
	logger.Debugf("Status triggered: %v", req)
	// Use vendored strings
	runtimeReadyConditionString := rtApi.RuntimeReady
	networkReadyConditionString := rtApi.NetworkReady

	response := &rtApi.StatusResponse{
		Status: &rtApi.RuntimeStatus{
			Conditions: []*rtApi.RuntimeCondition{
				{
					Type:   runtimeReadyConditionString,
					Status: true,
				},
				{
					Type:   networkReadyConditionString,
					Status: true,
				},
			},
		},
	}

	logger.Debugf("Status responded: %v", response)
	return response, nil
}
