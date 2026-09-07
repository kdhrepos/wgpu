package driver

import "gitlab.com/akita/akita"

// KernelCompletionObserver is an optional hook used by the SAC platform.
// The driver notifies it whenever a LaunchKernelReq completes (the kernel
// has finished executing on the GPU).
type KernelCompletionObserver interface {
	OnKernelComplete(now akita.VTimeInSec)
}

// SetKernelCompletionObserver registers the SAC kernel-completion observer.
// Nil (the default) disables all SAC-related behavior.
func (d *Driver) SetKernelCompletionObserver(o KernelCompletionObserver) {
	d.kernelCompletionObserver = o
}
