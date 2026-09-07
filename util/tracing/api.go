package tracing

import (
	"reflect"
	"sync"

	"gitlab.com/akita/akita"
)

// taskPool recycles Task objects so passing one through HookCtx.Item does not
// heap-allocate on every traced event. Consumers (traceHook) copy by value, so
// a Task is safe to reuse once InvokeHook returns.
var taskPool = sync.Pool{New: func() interface{} { return &Task{} }}

// NamedHookable represent something both have a name and can be hooked
type NamedHookable interface {
	akita.Named
	akita.Hookable
	InvokeHook(akita.HookCtx)
}

// hookCounter is satisfied by anything embedding akita.HookableBase. It lets
// the tracing entry points skip building a Task when no hook would observe it.
type hookCounter interface {
	NumHooks() int
}

// noHooks reports whether the domain has no hooks, so no Task need be built.
func noHooks(domain NamedHookable) bool {
	hc, ok := domain.(hookCounter)
	return ok && hc.NumHooks() == 0
}

// A list of hook poses for the hooks to apply to
var (
	HookPosTaskStart = &akita.HookPos{Name: "HookPosTaskStart"}
	HookPosTaskStep  = &akita.HookPos{Name: "HookPosTaskStep"}
	HookPosTaskEnd   = &akita.HookPos{Name: "HookPosTaskEnd"}
)

// StartTask notifies the hooks that hook to the domain about the start of a
// task.
func StartTask(
	id uint64,
	parentID uint64,
	now akita.VTimeInSec,
	domain NamedHookable,
	kind string,
	what string,
	detail interface{},
) {
	if noHooks(domain) {
		return
	}
	task := taskPool.Get().(*Task)
	*task = Task{
		ID:        id,
		ParentID:  parentID,
		StartTime: now,
		Kind:      kind,
		What:      what,
		Where:     domain.Name(),
		Detail:    detail,
	}
	domain.InvokeHook(akita.HookCtx{
		Now:    now,
		Domain: domain,
		Item:   task,
		Pos:    HookPosTaskStart,
	})
	*task = Task{}
	taskPool.Put(task)
}

// AddTaskStep marks that a milestone has been reached when processing a task.
func AddTaskStep(
	id uint64,
	now akita.VTimeInSec,
	domain NamedHookable,
	what string,
) {
	if noHooks(domain) {
		return
	}
	task := taskPool.Get().(*Task)
	*task = Task{
		ID:    id,
		Steps: []TaskStep{{Time: now, What: what}},
	}
	domain.InvokeHook(akita.HookCtx{
		Now:    now,
		Domain: domain,
		Item:   task,
		Pos:    HookPosTaskStep,
	})
	*task = Task{}
	taskPool.Put(task)
}

// EndTask notifies the hooks about the end of a task.
func EndTask(
	id uint64,
	now akita.VTimeInSec,
	domain NamedHookable,
) {
	if noHooks(domain) {
		return
	}
	task := taskPool.Get().(*Task)
	*task = Task{
		ID:      id,
		EndTime: now,
		Where:   domain.Name(),
	}
	domain.InvokeHook(akita.HookCtx{
		Now:    now,
		Domain: domain,
		Item:   task,
		Pos:    HookPosTaskEnd,
	})
	*task = Task{}
	taskPool.Put(task)
}

// MsgIDAtReceiver generates a standard ID for the message task at the
// message receiver.
func MsgIDAtReceiver(msg akita.Msg, domain NamedHookable) uint64 {
	return encodeTaskID(msg.Meta().ID, kindReqIn, domainID(domain.Name()))
}

// TraceReqInitiate generates a new task. The new task has Type="req_out",
// What=[the type name of the message]. This function is to be called by the
// sender of the message.
func TraceReqInitiate(
	msg akita.Msg,
	now akita.VTimeInSec,
	domain NamedHookable,
	taskParentID uint64,
) {
	StartTask(
		ReqOutTaskID(msg.Meta().ID),
		taskParentID,
		now,
		domain,
		"req_out",
		reflect.TypeOf(msg).String(),
		msg,
	)
}

func StartTracingNetworkReq(
	req akita.Msg,
	now akita.VTimeInSec,
	domain NamedHookable,
	parent akita.Msg,
) {
	StartTask(TransReqTaskID(req.Meta().ID),
		MsgIDAtReceiver(parent, domain),
		now, domain, "trace-trans-req",
		reflect.TypeOf(req).String(), req)
}

func StopTracingNetworkReq(
	req akita.Msg,
	now akita.VTimeInSec,
	domain NamedHookable,
) {
	EndTask(TransReqTaskID(req.Meta().ID), now, domain)
}

// TraceReqReceive generates a new task for the message handling. The kind of
// the task is always "req_in".
func TraceReqReceive(
	msg akita.Msg,
	now akita.VTimeInSec,
	domain NamedHookable,
) {
	StartTask(
		MsgIDAtReceiver(msg, domain),
		ReqOutTaskID(msg.Meta().ID),
		now,
		domain,
		"req_in",
		reflect.TypeOf(msg).String(),
		msg,
	)
}

// TraceReqComplete terminates the message handling task.
func TraceReqComplete(
	msg akita.Msg,
	now akita.VTimeInSec,
	domain NamedHookable,
) {
	EndTask(MsgIDAtReceiver(msg, domain), now, domain)
}

// TraceReqFinalize terminates the message task. This function should be called
// when the sender receives the response.
func TraceReqFinalize(
	msg akita.Msg,
	now akita.VTimeInSec,
	domain NamedHookable,
) {
	EndTask(ReqOutTaskID(msg.Meta().ID), now, domain)
}
