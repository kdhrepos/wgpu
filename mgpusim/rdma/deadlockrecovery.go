package rdma

import "gitlab.com/akita/akita"

func (e *RDMAEngineImpl) PushToRetryPort(
	msg akita.Msg,
) {
	e.retryPort.(*akita.LimitNumMsgPort).Push(msg)
}
