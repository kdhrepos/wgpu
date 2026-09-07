package idealmemcontroller

import "gitlab.com/akita/akita"

type ToDramMsg struct {
	Msg       akita.Msg
	FromLocal bool
}

func (m *ToDramMsg) Meta() *akita.MsgMeta {
	return m.Msg.Meta()
}
