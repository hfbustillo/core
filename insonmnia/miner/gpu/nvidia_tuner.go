// +build !darwin,cl

package gpu

import (
	"context"
	"net"
	"os"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-plugins-helpers/volume"
	log "github.com/noxiouz/zapctx/ctxlog"
	"github.com/sshaman1101/nvidia-docker/nvidia"
	"go.uber.org/zap"
)

type nvidiaTuner struct {
	volumePluginHandler
}

func (g *nvidiaTuner) Tune(hostconfig *container.HostConfig) error {
	// NOTE: driver name depends on UNIX socket name which Docker uses to connect to a driver
	hostconfig.VolumeDriver = g.options.volumeDriverName
	hostconfig.Binds = append(hostconfig.Binds, g.options.volumeName()+":/usr/local/nvidia:ro")

	if g.OpenCLVendorDir != "" {
		hostconfig.Binds = append(hostconfig.Binds, g.OpenCLVendorDir+":"+g.OpenCLVendorDir+":ro")
	}

	for _, device := range g.devices {
		hostconfig.Devices = append(hostconfig.Devices, container.DeviceMapping{
			PathOnHost:        device,
			PathInContainer:   device,
			CgroupPermissions: "rwm",
		})
	}
	return nil
}

func (g *nvidiaTuner) Close() error {
	if err := g.listener.Close(); err != nil {
		return err
	}
	return os.Remove(g.options.socketPath())
}

func newNvidiaTuner(ctx context.Context, opts *tunerOptions) (Tuner, error) {
	ovs := nvidiaTuner{}
	ovs.options = opts

	// Detect if we support NVIDIA
	log.G(ctx).Info("Loading NVIDIA unified memory")
	UVMErr := nvidia.LoadUVM()
	if UVMErr != nil {
		log.G(ctx).Warn("failed to load UVM. Seems NVIDIA is not installed on the host", zap.Error(UVMErr))
	}

	log.G(ctx).Info("Loading NVIDIA management library")
	initErr := nvidia.Init()
	if initErr == nil {
		defer func() { nvidia.Shutdown() }()
	}

	var nvidiaSupported = initErr == nil && UVMErr == nil
	if nvidiaSupported {
		log.G(ctx).Info("NVIDIA GPU supported by the host. Discovering GPU devices")
		devices, err := nvidia.LookupDevices()
		if err != nil {
			log.G(ctx).Error("failed to lookup GPU devices", zap.Error(err))
			return nil, err
		}
		cdevices, err := nvidia.GetControlDevicePaths()
		if err != nil {
			log.G(ctx).Error("failed to get contorl devices paths", zap.Error(err))
			return nil, err
		}
		ovs.devices = append(ovs.devices, cdevices...)
		for _, device := range devices {
			ovs.devices = append(ovs.devices, device.Path)
		}
	}

	if _, err := os.Stat("/dev/dri"); err == nil {
		ovs.devices = append(ovs.devices, "/dev/dri")
	}

	if _, err := os.Stat(openCLVendorDir); err == nil {
		ovs.OpenCLVendorDir = openCLVendorDir
	}

	volInfo := []nvidia.VolumeInfo{
		{
			Name:         "nvidia_driver",
			Mountpoint:   "/usr/local/nvidia",
			MountOptions: "ro",
			Components: map[string][]string{
				"binaries": {
					"nvidia-cuda-mps-control", // Multi process service CLI
					"nvidia-cuda-mps-server",  // Multi process service server
					"nvidia-debugdump",        // GPU coredump utility
					"nvidia-persistenced",     // Persistence mode utility
					"nvidia-smi",              // System management interface
				},
				"libraries": {
					// ----- Compute -----
					"libnvidia-ml.so",              // Management library
					"libcuda.so",                   // CUDA driver library
					"libnvidia-ptxjitcompiler.so",  // PTX-SASS JIT compiler (used by libcuda)
					"libnvidia-fatbinaryloader.so", // fatbin loader (used by libcuda)
					"libnvidia-opencl.so",          // NVIDIA OpenCL ICD
					"libnvidia-compiler.so",        // NVVM-PTX compiler for OpenCL (used by libnvidia-opencl)
					"libOpenCL.so",                 // OpenCL ICD loader

					// ------ Video ------
					"libvdpau_nvidia.so",  // NVIDIA VDPAU ICD
					"libnvidia-encode.so", // Video encoder
					"libnvcuvid.so",       // Video decoder
					"libnvidia-fbc.so",    // Framebuffer capture
					"libnvidia-ifr.so",    // OpenGL framebuffer capture

					// ----- Graphic -----
					"libGL.so",         // OpenGL/GLX legacy _or_ compatibility wrapper (GLVND)
					"libGLX.so",        // GLX ICD loader (GLVND)
					"libOpenGL.so",     // OpenGL ICD loader (GLVND)
					"libGLESv1_CM.so",  // OpenGL ES v1 common profile legacy _or_ ICD loader (GLVND)
					"libGLESv2.so",     // OpenGL ES v2 legacy _or_ ICD loader (GLVND)
					"libEGL.so",        // EGL ICD loader
					"libGLdispatch.so", // OpenGL dispatch (GLVND) (used by libOpenGL, libEGL and libGLES*)

					"libGLX_nvidia.so",         // OpenGL/GLX ICD (GLVND)
					"libEGL_nvidia.so",         // EGL ICD (GLVND)
					"libGLESv2_nvidia.so",      // OpenGL ES v2 ICD (GLVND)
					"libGLESv1_CM_nvidia.so",   // OpenGL ES v1 common profile ICD (GLVND)
					"libnvidia-eglcore.so",     // EGL core (used by libGLES* or libGLES*_nvidia and libEGL_nvidia)
					"libnvidia-egl-wayland.so", // EGL wayland extensions (used by libEGL_nvidia)
					"libnvidia-glcore.so",      // OpenGL core (used by libGL or libGLX_nvidia)
					"libnvidia-tls.so",         // Thread local storage (used by libGL or libGLX_nvidia)
					"libnvidia-glsi.so",        // OpenGL system interaction (used by libEGL_nvidia)
				},
			},
		},
	}

	log.G(ctx).Info("Provisioning volumes", zap.String("at", ovs.options.volumePath()))
	volumes, err := nvidia.LookupVolumes(ovs.options.volumePath(), ovs.options.driverVersion, volInfo)
	if err != nil {
		return nil, err
	}

	ovs.handler = volume.NewHandler(NewPlugin(volumes))
	ovs.listener, err = net.Listen("unix", ovs.options.socketPath())

	if err != nil {
		log.G(ctx).Error("failed to create listening socket for to communicate with Docker as plugin",
			zap.String("path", ovs.options.socketPath()), zap.Error(err))
		return nil, err
	}

	go func() {
		ovs.handler.Serve(ovs.listener)
	}()

	return &ovs, nil
}
