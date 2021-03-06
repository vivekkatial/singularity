// Copyright (c) 2018-2020, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sylabs/singularity/internal/pkg/instance"
	fakerootConfig "github.com/sylabs/singularity/internal/pkg/runtime/engine/fakeroot/config"
	"github.com/sylabs/singularity/internal/pkg/sylog"
	"github.com/sylabs/singularity/internal/pkg/util/priv"
	"github.com/sylabs/singularity/internal/pkg/util/starter"
	"github.com/sylabs/singularity/pkg/runtime/engine/config"
	"github.com/sylabs/singularity/pkg/util/crypt"
)

// CleanupContainer is called from master after the MonitorContainer returns.
// It is responsible for ensuring that the container has been properly torn down.
//
// Additional privileges may be gained when running
// in suid flow. However, when a user namespace is requested and it is not
// a hybrid workflow (e.g. fakeroot), then there is no privileged saved uid
// and thus no additional privileges can be gained.
//
// For better understanding of runtime flow in general refer to
// https://github.com/opencontainers/runtime-spec/blob/master/runtime.md#lifecycle.
// CleanupContainer is performing step 8/9 here.
func (e *EngineOperations) CleanupContainer(ctx context.Context, fatal error, status syscall.WaitStatus) error {
	// firstly stop all fuse drivers before any image removal
	// by image driver interruption or image cleanup for hybrid
	// fakeroot workflow
	e.stopFuseDrivers()

	if imageDriver != nil {
		if err := umount(true); err != nil {
			sylog.Errorf("%s", err)
		}
		if err := imageDriver.Stop(); err != nil {
			sylog.Errorf("could not stop driver: %s", err)
		}
	}

	if e.EngineConfig.GetDeleteImage() {
		image := e.EngineConfig.GetImage()
		sylog.Verbosef("Removing image %s", image)
		sylog.Infof("Cleaning up image...")

		var err error

		if e.EngineConfig.GetFakeroot() && os.Getuid() != 0 {
			// this is required when we are using SUID workflow
			// because master process is not in the fakeroot
			// context and can get permission denied error during
			// image removal, so we execute "rm -rf /tmp/image" via
			// the fakeroot engine
			err = fakerootCleanup(image)
		} else {
			err = os.RemoveAll(image)
		}
		if err != nil {
			sylog.Errorf("failed to delete container image %s: %s", image, err)
		}
	}

	if networkSetup != nil {
		if e.EngineConfig.GetFakeroot() {
			priv.Escalate()
		}
		if err := networkSetup.DelNetworks(ctx); err != nil {
			sylog.Errorf("could not delete networks: %v", err)
		}
		if e.EngineConfig.GetFakeroot() {
			priv.Drop()
		}
	}

	if cgroupManager != nil {
		if err := cgroupManager.Remove(); err != nil {
			sylog.Errorf("could not remove cgroups: %v", err)
		}
	}

	if cryptDev != "" && imageDriver == nil {
		if err := cleanupCrypt(cryptDev); err != nil {
			sylog.Errorf("could not cleanup crypt: %v", err)
		}
	}

	if e.EngineConfig.GetInstance() {
		file, err := instance.Get(e.CommonConfig.ContainerID, instance.SingSubDir)
		if err != nil {
			return err
		}
		return file.Delete()
	}

	return nil
}

func umount(escalate bool) error {
	if escalate {
		// elevate the privilege to unmount
		priv.Escalate()
		defer priv.Drop()
	}

	for i := len(umountPoints) - 1; i >= 0; i-- {
		p := umountPoints[i]
		err := syscall.Unmount(p, 0)
		// ignore EINVAL meaning it's not a mount point
		if err != nil && err.(syscall.Errno) != syscall.EINVAL {
			return fmt.Errorf("while unmounting %s directory: %s", p, err)
		}
	}

	return nil
}

func cleanupCrypt(path string) error {
	priv.Escalate()
	defer priv.Drop()

	if err := umount(false); err != nil {
		return err
	}

	devName := filepath.Base(path)

	cryptDev := &crypt.Device{}
	if err := cryptDev.CloseCryptDevice(devName); err != nil {
		return fmt.Errorf("unable to delete crypt device: %s", devName)
	}

	return nil
}

func fakerootCleanup(path string) error {
	command := []string{"/bin/rm", "-rf", path}

	sylog.Debugf("Calling fakeroot engine to execute %q", strings.Join(command, " "))

	cfg := &config.Common{
		EngineName:   fakerootConfig.Name,
		ContainerID:  "fakeroot",
		EngineConfig: &fakerootConfig.EngineConfig{Args: command},
	}

	return starter.Run(
		"Singularity fakeroot",
		cfg,
		starter.UseSuid(true),
	)
}
