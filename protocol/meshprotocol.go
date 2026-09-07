package protocol

import "gitlab.com/akita/akita"

type NetworkMsg struct {
	Msg     akita.Msg
	SrcPort akita.Port
}

func (m *NetworkMsg) Meta() *akita.MsgMeta {
	return m.Msg.Meta()
}
