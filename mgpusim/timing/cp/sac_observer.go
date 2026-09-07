package cp

import "gitlab.com/akita/akita"

// KernelObserver is an optional hook used by the SAC platform. The command
// processor notifies it when a kernel starts dispatching, and delays the
// next kernel launch while the observer reports it is not ready (e.g. an
// LLC reconfiguration flush is still in flight).
type KernelObserver interface {
	ReadyForKernel() bool
	OnKernelStart(now akita.VTimeInSec)
}

// SetKernelObserver registers the SAC kernel observer. Nil (the default)
// disables all SAC-related behavior.
func (p *CommandProcessor) SetKernelObserver(o KernelObserver) {
	p.kernelObserver = o
}
