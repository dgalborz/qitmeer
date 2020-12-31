package cmds

type GetNodeInfoCmd struct{}

func NewGetNodeInfoCmd() *GetNodeInfoCmd {
	return &GetNodeInfoCmd{}
}

type GetPeerInfoCmd struct{}

func NewGetPeerInfoCmd() *GetPeerInfoCmd {
	return &GetPeerInfoCmd{}
}

type GetRpcInfoCmd struct{}

func NewGetRpcInfoCmd() *GetRpcInfoCmd {
	return &GetRpcInfoCmd{}
}

type GetTimeInfoCmd struct{}

func NewGetTimeInfoCmd() *GetTimeInfoCmd {
	return &GetTimeInfoCmd{}
}

type StopCmd struct{}

func NewStopCmd() *StopCmd {
	return &StopCmd{}
}

type BanlistCmd struct{}

func NewBanlistCmd() *BanlistCmd {
	return &BanlistCmd{}
}

type CheckAddressCmd struct {
	Address string
	Network string
}

func NewCheckAddressCmd(address string, network string) *CheckAddressCmd {
	return &CheckAddressCmd{
		Address: address,
		Network: network,
	}
}

func init() {
	flags := UsageFlag(0)

	MustRegisterCmd("getNodeInfo", (*GetNodeInfoCmd)(nil), flags, DefaultServiceNameSpace)
	MustRegisterCmd("getPeerInfo", (*GetPeerInfoCmd)(nil), flags, DefaultServiceNameSpace)
	MustRegisterCmd("getRpcInfo", (*GetRpcInfoCmd)(nil), flags, DefaultServiceNameSpace)
	MustRegisterCmd("getTimeInfo", (*GetTimeInfoCmd)(nil), flags, DefaultServiceNameSpace)
	MustRegisterCmd("stop", (*StopCmd)(nil), flags, TestNameSpace)
	MustRegisterCmd("banlist", (*BanlistCmd)(nil), flags, TestNameSpace)

	MustRegisterCmd("checkAddress", (*CheckAddressCmd)(nil), flags, DefaultServiceNameSpace)
}
