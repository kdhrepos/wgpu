package l1v

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/ca"
)

type bankActionType int

const (
	bankActionInvalid bankActionType = iota
	bankActionReadHit
	bankActionWrite
	bankActionWriteFetched
)

type transaction struct {
	id uint64

	read                *protocol.ReadReq
	readToBottom        *protocol.ReadReq
	dataReadyFromBottom *protocol.DataReadyRsp
	dataReadyToTop      *protocol.DataReadyRsp

	write          *protocol.WriteReq
	writeToBottom  *protocol.WriteReq
	doneFromBottom *protocol.WriteDoneRsp
	doneToTop      *protocol.WriteDoneRsp

	preCoalesceTransactions []*transaction

	bankAction            bankActionType
	block                 *cache.Block
	data                  []byte
	writeFetchedDirtyMask []bool

	fetchAndWrite bool
	bankDone      bool
	bottomDone    bool
	done          bool

	// failTraced ensures an MSHR reservation/merge fail is counted at most
	// once per transaction, so the metric reflects distinct blocked requests
	// rather than per-cycle Tick re-probes while the transaction stalls in
	// dirBuf.
	failTraced bool

	// mshrBlockedAt is the simulation time when this transaction first hit
	// a full MSHR (mshr-fail). Non-zero iff the transaction stalled waiting
	// for an MSHR slot; consumed once the slot is finally allocated to emit
	// the mshr-wait-time metric. Mirrors writeback's pattern.
	mshrBlockedAt akita.VTimeInSec
}

func (t *transaction) Address() uint64 {
	if t.read != nil {
		return t.read.Address
	}
	return t.write.Address
}

func (t *transaction) PID() ca.PID {
	return 1
	// if t.read != nil {
	// 	return t.read.PID
	// }
	// return t.write.PID
}
