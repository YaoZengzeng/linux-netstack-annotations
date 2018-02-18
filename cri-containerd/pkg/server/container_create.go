/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/contrib/apparmor"
	"github.com/containerd/containerd/contrib/seccomp"
	"github.com/containerd/containerd/linux/runcopts"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/typeurl"
	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/devices"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/runtime-tools/validate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/syndtr/gocapability/capability"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	"k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"

	customopts "github.com/kubernetes-incubator/cri-containerd/pkg/containerd/opts"
	cio "github.com/kubernetes-incubator/cri-containerd/pkg/server/io"
	containerstore "github.com/kubernetes-incubator/cri-containerd/pkg/store/container"
	"github.com/kubernetes-incubator/cri-containerd/pkg/util"
)

const (
	// profileNamePrefix is the prefix for loading profiles on a localhost. Eg. AppArmor localhost/profileName.
	profileNamePrefix = "localhost/" // TODO (mikebrow): get localhost/ & runtime/default from CRI kubernetes/kubernetes#51747
	// runtimeDefault indicates that we should use or create a runtime default profile.
	runtimeDefault = "runtime/default"
	// dockerDefault indicates that we should use or create a docker default profile.
	dockerDefault = "docker/default"
	// appArmorDefaultProfileName is name to use when creating a default apparmor profile.
	appArmorDefaultProfileName = "cri-containerd.apparmor.d"
	// unconfinedProfile is a string indicating one should run a pod/containerd without a security profile
	unconfinedProfile = "unconfined"
	// seccompDefaultProfile is the default seccomp profile.
	seccompDefaultProfile = dockerDefault
)

func init() {
	typeurl.Register(&containerstore.Metadata{},
		"github.com/kubernetes-incubator/cri-containerd/pkg/store/container", "Metadata")
}

// CreateContainer creates a new container in the given PodSandbox.
// 在给定的sandbox内创建一个新的容器
func (c *criContainerdService) CreateContainer(ctx context.Context, r *runtime.CreateContainerRequest) (_ *runtime.CreateContainerResponse, retErr error) {
	config := r.GetConfig()
	// 获取容器所属的sandbox的配置
	sandboxConfig := r.GetSandboxConfig()
	sandbox, err := c.sandboxStore.Get(r.GetPodSandboxId())
	if err != nil {
		return nil, fmt.Errorf("failed to find sandbox id %q: %v", r.GetPodSandboxId(), err)
	}
	sandboxID := sandbox.ID
	// 获取sandbox container的task
	s, err := sandbox.Container.Task(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox container task: %v", err)
	}
	// 获取sandbox所在容器的pid
	sandboxPid := s.Pid()

	// Generate unique id and name for the container and reserve the name.
	// Reserve the container name to avoid concurrent `CreateContainer` request creating
	// the same container.
	// 创建container ID和container name
	id := util.GenerateID()
	name := makeContainerName(config.GetMetadata(), sandboxConfig.GetMetadata())
	glog.V(4).Infof("Generated id %q for container %q", id, name)
	if err = c.containerNameIndex.Reserve(name, id); err != nil {
		return nil, fmt.Errorf("failed to reserve container name %q: %v", name, err)
	}
	defer func() {
		// Release the name if the function returns with an error.
		if retErr != nil {
			c.containerNameIndex.ReleaseByName(name)
		}
	}()

	// Create initial internal container metadata.
	// 创建容器的元数据
	meta := containerstore.Metadata{
		ID:        id,
		Name:      name,
		SandboxID: sandboxID,
		Config:    config,
	}

	// Prepare container image snapshot. For container, the image should have
	// been pulled before creating the container, so do not ensure the image.
	// 准备容器镜像的snapshot，对于容器，镜像需要在容器创建之前就已经被拉取
	imageRef := config.GetImage().GetImage()
	image, err := c.localResolve(ctx, imageRef)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve image %q: %v", imageRef, err)
	}
	if image == nil {
		return nil, fmt.Errorf("image %q not found", imageRef)
	}

	// Create container root directory.
	// 创建container的root目录，/var/lib/cri-containerd/containers/id
	containerRootDir := getContainerRootDir(c.config.RootDir, id)
	if err = c.os.MkdirAll(containerRootDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create container root directory %q: %v",
			containerRootDir, err)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the container root directory.
			if err = c.os.RemoveAll(containerRootDir); err != nil {
				glog.Errorf("Failed to remove container root directory %q: %v",
					containerRootDir, err)
			}
		}
	}()

	// Create container volumes mounts.
	// 创建容器的volume mounts，返回的是runtime.Mount
	// TODO(random-liu): Add cri-containerd integration test for image volume.
	volumeMounts := c.generateVolumeMounts(containerRootDir, config.GetMounts(), image.Config)

	// Generate container runtime spec.
	mounts := c.generateContainerMounts(getSandboxRootDir(c.config.RootDir, sandboxID), config)

	// 创建container spec
	spec, err := c.generateContainerSpec(id, sandboxPid, config, sandboxConfig, image.Config, append(mounts, volumeMounts...))
	if err != nil {
		return nil, fmt.Errorf("failed to generate container %q spec: %v", id, err)
	}
	glog.V(4).Infof("Container %q spec: %#+v", id, spew.NewFormatter(spec))

	// Set snapshotter before any other options.
	// 首先设置snapshotter
	opts := []containerd.NewContainerOpts{
		containerd.WithSnapshotter(c.config.ContainerdConfig.Snapshotter),
		customopts.WithImageUnpack(image.Image),
		// Prepare container rootfs. This is always writeable even if
		// the container wants a readonly rootfs since we want to give
		// the runtime (runc) a chance to modify (e.g. to create mount
		// points corresponding to spec.Mounts) before making the
		// rootfs readonly (requested by spec.Root.Readonly).
		// 准备容器的rootfs，因为我们希望它总是可写的，即使容器只想要一个可读的rootfs
		// 因为我们想让runtime有机会在让容器rootfs可读之前进行修改，比如创建spec.Mounts
		// 对应的挂载点
		containerd.WithNewSnapshot(id, image.Image),
	}

	if len(volumeMounts) > 0 {
		mountMap := make(map[string]string)
		for _, v := range volumeMounts {
			mountMap[v.HostPath] = v.ContainerPath
		}
		opts = append(opts, customopts.WithVolumes(mountMap))
	}
	meta.ImageRef = image.ID

	// Get container log path.
	if config.GetLogPath() != "" {
		meta.LogPath = filepath.Join(sandbox.Config.GetLogDirectory(), config.GetLogPath())
	}

	// 创建容器io，io是独立创建的
	containerIO, err := cio.NewContainerIO(id,
		cio.WithNewFIFOs(containerRootDir, config.GetTty(), config.GetStdin()))
	if err != nil {
		return nil, fmt.Errorf("failed to create container io: %v", err)
	}
	defer func() {
		if retErr != nil {
			if err := containerIO.Close(); err != nil {
				glog.Errorf("Failed to close container io %q : %v", id, err)
			}
		}
	}()

	// 创建SpecOpts
	var specOpts []containerd.SpecOpts
	securityContext := config.GetLinux().GetSecurityContext()
	// Set container username. This could only be done by containerd, because it needs
	// access to the container rootfs. Pass user name to containerd, and let it overwrite
	// the spec for us.
	if uid := securityContext.GetRunAsUser(); uid != nil {
		specOpts = append(specOpts, containerd.WithUserID(uint32(uid.GetValue())))
	}
	if username := securityContext.GetRunAsUsername(); username != "" {
		specOpts = append(specOpts, containerd.WithUsername(username))
	}

	apparmorSpecOpts, err := generateApparmorSpecOpts(
		securityContext.GetApparmorProfile(),
		securityContext.GetPrivileged(),
		c.apparmorEnabled)
	if err != nil {
		return nil, fmt.Errorf("failed to generate apparmor spec opts: %v", err)
	}
	if apparmorSpecOpts != nil {
		specOpts = append(specOpts, apparmorSpecOpts)
	}

	seccompSpecOpts, err := generateSeccompSpecOpts(
		securityContext.GetSeccompProfilePath(),
		securityContext.GetPrivileged(),
		c.seccompEnabled)
	if err != nil {
		return nil, fmt.Errorf("failed to generate seccomp spec opts: %v", err)
	}
	if seccompSpecOpts != nil {
		specOpts = append(specOpts, seccompSpecOpts)
	}
	// containerKindContainer是常量"container"，代表的是创建application container
	containerLabels := buildLabels(config.Labels, containerKindContainer)

	opts = append(opts,
		// specOpts通过WithSpec加入spec中
		containerd.WithSpec(spec, specOpts...),
		containerd.WithRuntime(
			c.config.ContainerdConfig.Runtime,
			&runcopts.RuncOptions{
				Runtime:       c.config.ContainerdConfig.RuntimeEngine,
				RuntimeRoot:   c.config.ContainerdConfig.RuntimeRoot,
				SystemdCgroup: c.config.SystemdCgroup}), // TODO (mikebrow): add CriuPath when we add support for pause
		containerd.WithContainerLabels(containerLabels),
		containerd.WithContainerExtension(containerMetadataExtension, &meta))
	var cntr containerd.Container
	// 调用containerd创建新的container
	if cntr, err = c.client.NewContainer(ctx, id, opts...); err != nil {
		return nil, fmt.Errorf("failed to create containerd container: %v", err)
	}
	defer func() {
		if retErr != nil {
			if err := cntr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
				glog.Errorf("Failed to delete containerd container %q: %v", id, err)
			}
		}
	}()

	status := containerstore.Status{CreatedAt: time.Now().UnixNano()}
	// 创建containerstore的container对象
	container, err := containerstore.NewContainer(meta,
		// 将status写入文件
		containerstore.WithStatus(status, containerRootDir),
		containerstore.WithContainer(cntr),
		// 容器io独立创建并保存
		containerstore.WithContainerIO(containerIO),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create internal container object for %q: %v",
			id, err)
	}
	defer func() {
		if retErr != nil {
			// Cleanup container checkpoint on error.
			if err := container.Delete(); err != nil {
				glog.Errorf("Failed to cleanup container checkpoint for %q: %v", id, err)
			}
		}
	}()

	// Add container into container store.
	if err := c.containerStore.Add(container); err != nil {
		return nil, fmt.Errorf("failed to add container %q into store: %v", id, err)
	}

	return &runtime.CreateContainerResponse{ContainerId: id}, nil
}

func (c *criContainerdService) generateContainerSpec(id string, sandboxPid uint32, config *runtime.ContainerConfig,
	sandboxConfig *runtime.PodSandboxConfig, imageConfig *imagespec.ImageConfig, extraMounts []*runtime.Mount) (*runtimespec.Spec, error) {
	// Creates a spec Generator with the default spec.
	// 创建一个有默认spec的spec generator
	spec, err := defaultRuntimeSpec(id)
	if err != nil {
		return nil, err
	}
	g := generate.NewFromSpec(spec)

	// Set the relative path to the rootfs of the container from containerd's
	// pre-defined directory.
	g.SetRootPath(relativeRootfsPath)

	if err := setOCIProcessArgs(&g, config, imageConfig); err != nil {
		return nil, err
	}

	if config.GetWorkingDir() != "" {
		g.SetProcessCwd(config.GetWorkingDir())
	} else if imageConfig.WorkingDir != "" {
		g.SetProcessCwd(imageConfig.WorkingDir)
	}

	g.SetProcessTerminal(config.GetTty())
	if config.GetTty() {
		g.AddProcessEnv("TERM", "xterm")
	}

	// Apply envs from image config first, so that envs from container config
	// can override them.
	// 首先应用image config，从而能让container config中的env覆盖它们
	if err := addImageEnvs(&g, imageConfig.Env); err != nil {
		return nil, err
	}
	for _, e := range config.GetEnvs() {
		g.AddProcessEnv(e.GetKey(), e.GetValue())
	}

	securityContext := config.GetLinux().GetSecurityContext()
	selinuxOpt := securityContext.GetSelinuxOptions()
	processLabel, mountLabel, err := initSelinuxOpts(selinuxOpt)
	if err != nil {
		return nil, fmt.Errorf("failed to init selinux options %+v: %v", securityContext.GetSelinuxOptions(), err)
	}

	// Add extra mounts first so that CRI specified mounts can override.
	mounts := append(extraMounts, config.GetMounts()...)
	if err := c.addOCIBindMounts(&g, mounts, mountLabel); err != nil {
		return nil, fmt.Errorf("failed to set OCI bind mounts %+v: %v", mounts, err)
	}

	if securityContext.GetPrivileged() {
		if !securityContext.GetPrivileged() {
			return nil, fmt.Errorf("no privileged container allowed in sandbox")
		}
		if err := setOCIPrivileged(&g, config); err != nil {
			return nil, err
		}
	} else { // not privileged
		if err := c.addOCIDevices(&g, config.GetDevices()); err != nil {
			return nil, fmt.Errorf("failed to set devices mapping %+v: %v", config.GetDevices(), err)
		}

		if err := setOCICapabilities(&g, securityContext.GetCapabilities()); err != nil {
			return nil, fmt.Errorf("failed to set capabilities %+v: %v",
				securityContext.GetCapabilities(), err)
		}
	}

	g.SetProcessSelinuxLabel(processLabel)
	g.SetLinuxMountLabel(mountLabel)

	// TODO: Figure out whether we should set no new privilege for sandbox container by default
	g.SetProcessNoNewPrivileges(securityContext.GetNoNewPrivs())

	// TODO(random-liu): [P1] Set selinux options (privileged or not).

	g.SetRootReadonly(securityContext.GetReadonlyRootfs())

	setOCILinuxResource(&g, config.GetLinux().GetResources())

	if sandboxConfig.GetLinux().GetCgroupParent() != "" {
		cgroupsPath := getCgroupsPath(sandboxConfig.GetLinux().GetCgroupParent(), id,
			c.config.SystemdCgroup)
		g.SetLinuxCgroupsPath(cgroupsPath)
	}

	// Set namespaces, share namespace with sandbox container.
	// 设置namespaces，和其他sandbox共享container
	setOCINamespaces(&g, securityContext.GetNamespaceOptions(), sandboxPid)

	supplementalGroups := securityContext.GetSupplementalGroups()
	for _, group := range supplementalGroups {
		g.AddProcessAdditionalGid(uint32(group))
	}

	return g.Spec(), nil
}

// generateVolumeMounts sets up image volumes for container. Rely on the removal of container
// root directory to do cleanup. Note that image volume will be skipped, if there is criMounts
// specified with the same destination.
// generateVolumeMounts设置容器的image volumes，依赖容器的根目录的删除来进行清除操作
func (c *criContainerdService) generateVolumeMounts(containerRootDir string, criMounts []*runtime.Mount, config *imagespec.ImageConfig) []*runtime.Mount {
	if len(config.Volumes) == 0 {
		return nil
	}
	var mounts []*runtime.Mount
	for dst := range config.Volumes {
		// 如果criMounts中也有指定了，则跳过image里的volume
		if isInCRIMounts(dst, criMounts) {
			// Skip the image volume, if there is CRI defined volume mapping.
			// TODO(random-liu): This should be handled by Kubelet in the future.
			// Kubelet should decide what to use for image volume, and also de-duplicate
			// the image volume and user mounts.
			continue
		}
		volumeID := util.GenerateID()
		src := filepath.Join(containerRootDir, "volumes", volumeID)
		// addOCIBindMounts will create these volumes.
		mounts = append(mounts, &runtime.Mount{
			ContainerPath: dst,
			HostPath:      src,
			// Use default mount propagation.
			// TODO(random-liu): What about selinux relabel?
		})
	}
	return mounts
}

// generateContainerMounts sets up necessary container mounts including /dev/shm, /etc/hosts
// and /etc/resolv.conf.
func (c *criContainerdService) generateContainerMounts(sandboxRootDir string, config *runtime.ContainerConfig) []*runtime.Mount {
	var mounts []*runtime.Mount
	securityContext := config.GetLinux().GetSecurityContext()
	if !isInCRIMounts(etcHosts, config.GetMounts()) {
		mounts = append(mounts, &runtime.Mount{
			ContainerPath: etcHosts,
			HostPath:      getSandboxHosts(sandboxRootDir),
			Readonly:      securityContext.GetReadonlyRootfs(),
		})
	}

	// Mount sandbox resolv.config.
	// TODO: Need to figure out whether we should always mount it as read-only
	if !isInCRIMounts(resolvConfPath, config.GetMounts()) {
		mounts = append(mounts, &runtime.Mount{
			ContainerPath: resolvConfPath,
			HostPath:      getResolvPath(sandboxRootDir),
			Readonly:      securityContext.GetReadonlyRootfs(),
		})
	}

	if !isInCRIMounts(devShm, config.GetMounts()) {
		sandboxDevShm := getSandboxDevShm(sandboxRootDir)
		if securityContext.GetNamespaceOptions().GetHostIpc() {
			sandboxDevShm = devShm
		}
		mounts = append(mounts, &runtime.Mount{
			ContainerPath: devShm,
			HostPath:      sandboxDevShm,
			Readonly:      false,
		})
	}
	return mounts
}

// setOCIProcessArgs sets process args. It returns error if the final arg list
// is empty.
func setOCIProcessArgs(g *generate.Generator, config *runtime.ContainerConfig, imageConfig *imagespec.ImageConfig) error {
	command, args := config.GetCommand(), config.GetArgs()
	// The following logic is migrated from https://github.com/moby/moby/blob/master/daemon/commit.go
	// TODO(random-liu): Clearly define the commands overwrite behavior.
	if len(command) == 0 {
		// Copy array to avoid data race.
		if len(args) == 0 {
			args = append([]string{}, imageConfig.Cmd...)
		}
		if command == nil {
			command = append([]string{}, imageConfig.Entrypoint...)
		}
	}
	if len(command) == 0 && len(args) == 0 {
		return fmt.Errorf("no command specified")
	}
	g.SetProcessArgs(append(command, args...))
	return nil
}

// addImageEnvs adds environment variables from image config. It returns error if
// an invalid environment variable is encountered.
func addImageEnvs(g *generate.Generator, imageEnvs []string) error {
	for _, e := range imageEnvs {
		kv := strings.SplitN(e, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("invalid environment variable %q", e)
		}
		g.AddProcessEnv(kv[0], kv[1])
	}
	return nil
}

func setOCIPrivileged(g *generate.Generator, config *runtime.ContainerConfig) error {
	// Add all capabilities in privileged mode.
	g.SetupPrivileged(true)
	setOCIBindMountsPrivileged(g)
	if err := setOCIDevicesPrivileged(g); err != nil {
		return fmt.Errorf("failed to set devices mapping %+v: %v", config.GetDevices(), err)
	}
	return nil
}

func clearReadOnly(m *runtimespec.Mount) {
	var opt []string
	for _, o := range m.Options {
		if o != "ro" {
			opt = append(opt, o)
		}
	}
	m.Options = opt
}

// addDevices set device mapping without privilege.
func (c *criContainerdService) addOCIDevices(g *generate.Generator, devs []*runtime.Device) error {
	spec := g.Spec()
	for _, device := range devs {
		path, err := c.os.ResolveSymbolicLink(device.HostPath)
		if err != nil {
			return err
		}
		dev, err := devices.DeviceFromPath(path, device.Permissions)
		if err != nil {
			return err
		}
		rd := runtimespec.LinuxDevice{
			Path:  device.ContainerPath,
			Type:  string(dev.Type),
			Major: dev.Major,
			Minor: dev.Minor,
			UID:   &dev.Uid,
			GID:   &dev.Gid,
		}
		g.AddDevice(rd)
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, runtimespec.LinuxDeviceCgroup{
			Allow:  true,
			Type:   string(dev.Type),
			Major:  &dev.Major,
			Minor:  &dev.Minor,
			Access: dev.Permissions,
		})
	}
	return nil
}

// addDevices set device mapping with privilege.
func setOCIDevicesPrivileged(g *generate.Generator) error {
	spec := g.Spec()
	hostDevices, err := devices.HostDevices()
	if err != nil {
		return err
	}
	for _, hostDevice := range hostDevices {
		rd := runtimespec.LinuxDevice{
			Path:  hostDevice.Path,
			Type:  string(hostDevice.Type),
			Major: hostDevice.Major,
			Minor: hostDevice.Minor,
			UID:   &hostDevice.Uid,
			GID:   &hostDevice.Gid,
		}
		if hostDevice.Major == 0 && hostDevice.Minor == 0 {
			// Invalid device, most likely a symbolic link, skip it.
			continue
		}
		g.AddDevice(rd)
	}
	spec.Linux.Resources.Devices = []runtimespec.LinuxDeviceCgroup{
		{
			Allow:  true,
			Access: "rwm",
		},
	}
	return nil
}

// addOCIBindMounts adds bind mounts.
func (c *criContainerdService) addOCIBindMounts(g *generate.Generator, mounts []*runtime.Mount, mountLabel string) error {
	// Mount cgroup into the container as readonly, which inherits docker's behavior.
	g.AddCgroupsMount("ro") // nolint: errcheck
	for _, mount := range mounts {
		dst := mount.GetContainerPath()
		src := mount.GetHostPath()
		// Create the host path if it doesn't exist.
		// TODO(random-liu): Add CRI validation test for this case.
		if _, err := c.os.Stat(src); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("failed to stat %q: %v", src, err)
			}
			if err := c.os.MkdirAll(src, 0755); err != nil {
				return fmt.Errorf("failed to mkdir %q: %v", src, err)
			}
		}
		// TODO(random-liu): Add cri-containerd integration test or cri validation test
		// for this.
		src, err := c.os.ResolveSymbolicLink(src)
		if err != nil {
			return fmt.Errorf("failed to resolve symlink %q: %v", src, err)
		}

		options := []string{"rbind"}
		switch mount.GetPropagation() {
		case runtime.MountPropagation_PROPAGATION_PRIVATE:
			options = append(options, "rprivate")
			// Since default root propogation in runc is rprivate ignore
			// setting the root propagation
		case runtime.MountPropagation_PROPAGATION_BIDIRECTIONAL:
			if err := ensureShared(src, c.os.LookupMount); err != nil {
				return err
			}
			options = append(options, "rshared")
			g.SetLinuxRootPropagation("rshared") // nolint: errcheck
		case runtime.MountPropagation_PROPAGATION_HOST_TO_CONTAINER:
			if err := ensureSharedOrSlave(src, c.os.LookupMount); err != nil {
				return err
			}
			options = append(options, "rslave")
			if g.Spec().Linux.RootfsPropagation != "rshared" &&
				g.Spec().Linux.RootfsPropagation != "rslave" {
				g.SetLinuxRootPropagation("rslave") // nolint: errcheck
			}
		default:
			glog.Warningf("Unknown propagation mode for hostPath %q", mount.HostPath)
			options = append(options, "rprivate")
		}

		// NOTE(random-liu): we don't change all mounts to `ro` when root filesystem
		// is readonly. This is different from docker's behavior, but make more sense.
		if mount.GetReadonly() {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}

		if mount.GetSelinuxRelabel() {
			if err := label.Relabel(src, mountLabel, true); err != nil && err != unix.ENOTSUP {
				return fmt.Errorf("relabel %q with %q failed: %v", src, mountLabel, err)
			}
		}
		g.AddBindMount(src, dst, options)
	}

	return nil
}

func setOCIBindMountsPrivileged(g *generate.Generator) {
	spec := g.Spec()
	// clear readonly for /sys and cgroup
	for i, m := range spec.Mounts {
		if spec.Mounts[i].Destination == "/sys" && !spec.Root.Readonly {
			clearReadOnly(&spec.Mounts[i])
		}
		if m.Type == "cgroup" {
			clearReadOnly(&spec.Mounts[i])
		}
	}
	spec.Linux.ReadonlyPaths = nil
	spec.Linux.MaskedPaths = nil
}

// setOCILinuxResource set container resource limit.
func setOCILinuxResource(g *generate.Generator, resources *runtime.LinuxContainerResources) {
	if resources == nil {
		return
	}
	g.SetLinuxResourcesCPUPeriod(uint64(resources.GetCpuPeriod()))
	g.SetLinuxResourcesCPUQuota(resources.GetCpuQuota())
	g.SetLinuxResourcesCPUShares(uint64(resources.GetCpuShares()))
	g.SetLinuxResourcesMemoryLimit(resources.GetMemoryLimitInBytes())
	g.SetProcessOOMScoreAdj(int(resources.GetOomScoreAdj()))
	g.SetLinuxResourcesCPUCpus(resources.GetCpusetCpus())
	g.SetLinuxResourcesCPUMems(resources.GetCpusetMems())
}

// getOCICapabilitiesList returns a list of all available capabilities.
func getOCICapabilitiesList() []string {
	var caps []string
	for _, cap := range capability.List() {
		if cap > validate.LastCap() {
			continue
		}
		caps = append(caps, "CAP_"+strings.ToUpper(cap.String()))
	}
	return caps
}

// setOCICapabilities adds/drops process capabilities.
func setOCICapabilities(g *generate.Generator, capabilities *runtime.Capability) error {
	if capabilities == nil {
		return nil
	}

	// Add/drop all capabilities if "all" is specified, so that
	// following individual add/drop could still work. E.g.
	// AddCapabilities: []string{"ALL"}, DropCapabilities: []string{"CHOWN"}
	// will be all capabilities without `CAP_CHOWN`.
	if util.InStringSlice(capabilities.GetAddCapabilities(), "ALL") {
		for _, c := range getOCICapabilitiesList() {
			if err := g.AddProcessCapability(c); err != nil {
				return err
			}
		}
	}
	if util.InStringSlice(capabilities.GetDropCapabilities(), "ALL") {
		for _, c := range getOCICapabilitiesList() {
			if err := g.DropProcessCapability(c); err != nil {
				return err
			}
		}
	}

	for _, c := range capabilities.GetAddCapabilities() {
		if strings.ToUpper(c) == "ALL" {
			continue
		}
		// Capabilities in CRI doesn't have `CAP_` prefix, so add it.
		if err := g.AddProcessCapability("CAP_" + strings.ToUpper(c)); err != nil {
			return err
		}
	}

	for _, c := range capabilities.GetDropCapabilities() {
		if strings.ToUpper(c) == "ALL" {
			continue
		}
		if err := g.DropProcessCapability("CAP_" + strings.ToUpper(c)); err != nil {
			return err
		}
	}
	return nil
}

// setOCINamespaces sets namespaces.
func setOCINamespaces(g *generate.Generator, namespaces *runtime.NamespaceOption, sandboxPid uint32) {
	// 共享network, ipc以及uts namespace
	g.AddOrReplaceLinuxNamespace(string(runtimespec.NetworkNamespace), getNetworkNamespace(sandboxPid)) // nolint: errcheck
	g.AddOrReplaceLinuxNamespace(string(runtimespec.IPCNamespace), getIPCNamespace(sandboxPid))         // nolint: errcheck
	g.AddOrReplaceLinuxNamespace(string(runtimespec.UTSNamespace), getUTSNamespace(sandboxPid))         // nolint: errcheck
	// Do not share pid namespace for now.
	// 暂时先不共享pid namespace
	if namespaces.GetHostPid() {
		g.RemoveLinuxNamespace(string(runtimespec.PIDNamespace)) // nolint: errcheck
	}
}

// defaultRuntimeSpec returns a default runtime spec used in cri-containerd.
func defaultRuntimeSpec(id string) (*runtimespec.Spec, error) {
	// GenerateSpec needs namespace.
	// k8sContainerdNamespace中表示的是我们用于连接containerd使用的namespace
	ctx := namespaces.WithNamespace(context.Background(), k8sContainerdNamespace)
	spec, err := containerd.GenerateSpec(ctx, nil, &containers.Container{ID: id})
	if err != nil {
		return nil, err
	}

	// Remove `/run` mount
	// TODO(random-liu): Mount tmpfs for /run and handle copy-up.
	// 去除`/run`的mount，在/run挂载tmpfs并且处理copy-up
	var mounts []runtimespec.Mount
	for _, mount := range spec.Mounts {
		if mount.Destination == "/run" {
			continue
		}
		mounts = append(mounts, mount)
	}
	spec.Mounts = mounts

	// Make sure no default seccomp/apparmor is specified
	// 确保不指定默认的seccomp/apparmor
	if spec.Process != nil {
		spec.Process.ApparmorProfile = ""
	}
	if spec.Linux != nil {
		spec.Linux.Seccomp = nil
	}
	return spec, nil
}

// generateSeccompSpecOpts generates containerd SpecOpts for seccomp.
func generateSeccompSpecOpts(seccompProf string, privileged, seccompEnabled bool) (containerd.SpecOpts, error) {
	if privileged {
		// Do not set seccomp profile when container is privileged
		return nil, nil
	}
	// Set seccomp profile
	if seccompProf == runtimeDefault || seccompProf == dockerDefault {
		// use correct default profile (Eg. if not configured otherwise, the default is docker/default)
		seccompProf = seccompDefaultProfile
	}
	if !seccompEnabled {
		if seccompProf != "" && seccompProf != unconfinedProfile {
			return nil, fmt.Errorf("seccomp is not supported")
		}
		return nil, nil
	}
	switch seccompProf {
	case "", unconfinedProfile:
		// Do not set seccomp profile.
		return nil, nil
	case dockerDefault:
		// Note: WithDefaultProfile specOpts must be added after capabilities
		return seccomp.WithDefaultProfile(), nil
	default:
		// Require and Trim default profile name prefix
		if !strings.HasPrefix(seccompProf, profileNamePrefix) {
			return nil, fmt.Errorf("invalid seccomp profile %q", seccompProf)
		}
		return seccomp.WithProfile(strings.TrimPrefix(seccompProf, profileNamePrefix)), nil
	}
}

// generateApparmorSpecOpts generates containerd SpecOpts for apparmor.
func generateApparmorSpecOpts(apparmorProf string, privileged, apparmorEnabled bool) (containerd.SpecOpts, error) {
	if !apparmorEnabled {
		// Should fail loudly if user try to specify apparmor profile
		// but we don't support it.
		// 如果用于要指定apparmor profile但是我们并不支持, 直接报错
		if apparmorProf != "" && apparmorProf != unconfinedProfile {
			return nil, fmt.Errorf("apparmor is not supported")
		}
		return nil, nil
	}
	switch apparmorProf {
	case runtimeDefault:
		// TODO (mikebrow): delete created apparmor default profile
		// 创建默认的profile name
		return apparmor.WithDefaultProfile(appArmorDefaultProfileName), nil
	case unconfinedProfile:
		return nil, nil
	case "":
		// Based on kubernetes#51746, default apparmor profile should be applied
		// for non-privileged container when apparmor is not specified.
		// 如果没有指定apparmor，default apparmor profile需要应用到non-privileged container
		if privileged {
			// 如果是privileged container直接返回nil
			return nil, nil
		}
		return apparmor.WithDefaultProfile(appArmorDefaultProfileName), nil
	default:
		// Require and Trim default profile name prefix
		if !strings.HasPrefix(apparmorProf, profileNamePrefix) {
			return nil, fmt.Errorf("invalid apparmor profile %q", apparmorProf)
		}
		// 默认添加指定的profile
		return apparmor.WithProfile(strings.TrimPrefix(apparmorProf, profileNamePrefix)), nil
	}
}

// Ensure mount point on which path is mounted, is shared.
func ensureShared(path string, lookupMount func(string) (mount.Info, error)) error {
	mountInfo, err := lookupMount(path)
	if err != nil {
		return err
	}

	// Make sure source mount point is shared.
	optsSplit := strings.Split(mountInfo.Optional, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		}
	}

	return fmt.Errorf("path %q is mounted on %q but it is not a shared mount", path, mountInfo.Mountpoint)
}

// Ensure mount point on which path is mounted, is either shared or slave.
func ensureSharedOrSlave(path string, lookupMount func(string) (mount.Info, error)) error {
	mountInfo, err := lookupMount(path)
	if err != nil {
		return err
	}
	// Make sure source mount point is shared.
	optsSplit := strings.Split(mountInfo.Optional, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		} else if strings.HasPrefix(opt, "master:") {
			return nil
		}
	}
	return fmt.Errorf("path %q is mounted on %q but it is not a shared or slave mount", path, mountInfo.Mountpoint)
}
