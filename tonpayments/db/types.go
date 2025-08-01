package db

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"strconv"
	"sync"
	"time"
)

type VirtualChannelEventType string

const (
	VirtualChannelEventTypeOpen     VirtualChannelEventType = "open"
	VirtualChannelEventTypeClose    VirtualChannelEventType = "close"
	VirtualChannelEventTypeTransfer VirtualChannelEventType = "transfer"
	VirtualChannelEventTypeRemove   VirtualChannelEventType = "remove"
)

type VirtualChannelEvent struct {
	EventType      VirtualChannelEventType `json:"event_type"`
	VirtualChannel any                     `json:"virtual_channel"`
}

type ChannelHistoryActionTransferInData struct {
	Amount string
	From   []byte
}

type ChannelHistoryActionTransferOutData struct {
	Amount string
	To     []byte
}

type ChannelHistoryActionAmountData struct {
	Amount string
}

type ChannelStatus uint8
type VirtualChannelStatus uint8
type ChannelHistoryEventType uint8

const (
	ChannelStateInactive ChannelStatus = iota
	ChannelStateActive
	ChannelStateClosing
	ChannelStateAny ChannelStatus = 100
)

const (
	ChannelHistoryActionTopup ChannelHistoryEventType = iota + 1
	ChannelHistoryActionTopupCapacity
	ChannelHistoryActionWithdraw
	ChannelHistoryActionWithdrawCapacity
	ChannelHistoryActionTransferIn
	ChannelHistoryActionTransferOut
	ChannelHistoryActionUncooperativeCloseStarted
	ChannelHistoryActionClosed
)

const (
	VirtualChannelStateActive VirtualChannelStatus = iota + 1
	VirtualChannelStateWantClose
	VirtualChannelStateClosed
	VirtualChannelStateWantRemove
	VirtualChannelStateRemoved
	VirtualChannelStatePending
)

var ErrAlreadyExists = errors.New("already exists")
var ErrNotFound = errors.New("not found")
var ErrChannelBusy = fmt.Errorf("channel is busy")

type VirtualChannelMetaSide struct {
	ChannelAddress        string
	Capacity              string
	Fee                   string
	UncooperativeDeadline time.Time
	SafeDeadline          time.Time
	SenderKey             []byte
}

type VirtualChannelMeta struct {
	Key              []byte
	Status           VirtualChannelStatus
	Incoming         *VirtualChannelMetaSide
	Outgoing         *VirtualChannelMetaSide
	LastKnownResolve []byte
	FinalDestination ed25519.PublicKey // known only to first initiator

	CreatedAt time.Time
	UpdatedAt time.Time
}

type ChannelHistoryItem struct {
	At     time.Time `json:"-"`
	Action ChannelHistoryEventType
	Data   json.RawMessage
}

type Channel struct {
	ID                     []byte
	Address                string
	ExtraCurrencyID        uint32
	JettonAddress          string
	Status                 ChannelStatus
	WeLeft                 bool
	OurOnchain             OnchainState
	TheirOnchain           OnchainState
	SafeOnchainClosePeriod int64

	AcceptingActions bool
	WebPeer          bool

	Our   Side
	Their Side

	// InitAt - initialization or reinitialization time
	InitAt          time.Time
	CreatedAt       time.Time
	LastProcessedLT uint64

	DBVersion int64

	mx sync.RWMutex
}

type OnchainState struct {
	Key            ed25519.PublicKey
	CommittedSeqno uint64
	WalletAddress  string
	Deposited      *big.Int
	Withdrawn      *big.Int
	Sent           *big.Int
}

type Side struct {
	payments.SignedSemiChannel
	Conditionals    *cell.Dictionary
	PendingWithdraw *big.Int
}

var ErrNewerStateIsKnown = errors.New("newer state is already known")

func NewSide(channelId []byte, seqno, counterpartySeqno uint64) Side {
	return Side{
		SignedSemiChannel: payments.SignedSemiChannel{
			Signature: payments.Signature{
				Value: make([]byte, 64),
			},
			State: payments.SemiChannel{
				ChannelID: channelId,
				Data: payments.SemiChannelBody{
					Seqno:            seqno,
					Sent:             tlb.ZeroCoins,
					ConditionalsHash: make([]byte, 32),
				},
				CounterpartyData: &payments.SemiChannelBody{
					Seqno:            counterpartySeqno,
					Sent:             tlb.ZeroCoins,
					ConditionalsHash: make([]byte, 32),
				},
			},
		},
		PendingWithdraw: big.NewInt(0),
	}
}

func (s *Side) IsReady() bool {
	return !bytes.Equal(s.Signature.Value, make([]byte, 64))
}

func (s *Side) Copy() *Side {
	sd := &Side{
		SignedSemiChannel: payments.SignedSemiChannel{
			Signature: payments.Signature{
				Value: append([]byte{}, s.Signature.Value...),
			},
			State: payments.SemiChannel{
				ChannelID: append([]byte{}, s.State.ChannelID...),
				Data: payments.SemiChannelBody{
					Seqno:            s.State.Data.Seqno,
					Sent:             s.State.Data.Sent,
					ConditionalsHash: s.State.Data.ConditionalsHash,
				},
			},
		},
		Conditionals:    s.Conditionals.Copy(),
		PendingWithdraw: s.PendingWithdraw,
	}

	if s.State.CounterpartyData != nil {
		sd.State.CounterpartyData = &payments.SemiChannelBody{
			Seqno:            s.State.CounterpartyData.Seqno,
			Sent:             s.State.CounterpartyData.Sent,
			ConditionalsHash: s.State.CounterpartyData.ConditionalsHash,
		}
	}

	return sd
}

func (s *Side) UnmarshalJSON(bytes []byte) error {
	str, err := strconv.Unquote(string(bytes))
	if err != nil {
		return err
	}

	data, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		return err
	}

	cl, err := cell.FromBOC(data)
	if err != nil {
		return err
	}

	sl := cl.BeginParse()
	ssc, err := sl.LoadRef()
	if err != nil {
		return err
	}

	s.Conditionals, err = sl.LoadDict(32)
	if err != nil {
		return err
	}

	s.PendingWithdraw, err = sl.LoadBigCoins()
	if err != nil {
		return err
	}

	return tlb.LoadFromCell(&s.SignedSemiChannel, ssc)
}

func (s *Side) MarshalJSON() ([]byte, error) {
	bts, err := tlb.ToCell(s.SignedSemiChannel)
	if err != nil {
		return nil, err
	}

	c := cell.BeginCell().MustStoreRef(bts).MustStoreDict(s.Conditionals).MustStoreBigCoins(s.PendingWithdraw).EndCell()
	return []byte(strconv.Quote(base64.StdEncoding.EncodeToString(c.ToBOC()))), nil
}

func (ch *Channel) CalcBalance(isTheir bool) (*big.Int, *big.Int, error) {
	// TODO: cache calculated

	ch.mx.RLock()
	defer ch.mx.RUnlock()

	s1, s1chain, s2, s2chain := ch.Our, ch.OurOnchain, ch.Their, ch.TheirOnchain
	if isTheir {
		s1, s2 = s2, s1
		s1chain, s2chain = s2chain, s1chain
	}

	maxWithdraw := s1chain.Withdrawn
	if maxWithdraw.Cmp(s1.PendingWithdraw) < 0 {
		maxWithdraw = s1.PendingWithdraw
	}

	balance := new(big.Int).Add(s2.State.Data.Sent.Nano(), new(big.Int).Sub(s1chain.Deposited, maxWithdraw))
	balance = balance.Sub(balance, s1.State.Data.Sent.Nano())
	
	locked := big.NewInt(0)
	if s1.PendingWithdraw.Sign() > 0 {
		locked = locked.Sub(s1.PendingWithdraw, s1chain.Withdrawn)
	}

	if s1.Conditionals.IsEmpty() {
		return balance, locked, nil
	}

	all, err := s1.Conditionals.LoadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load conditions: %w", err)
	}

	for _, kv := range all {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse condition %d: %w", kv.Key.MustLoadUInt(32), err)
		}
		balance = balance.Sub(balance, vch.Capacity)
		balance = balance.Sub(balance, vch.Fee)
		balance = balance.Add(balance, vch.Prepay)

		locked = locked.Add(locked, vch.Capacity)
		locked = locked.Add(locked, vch.Fee)
		locked = locked.Sub(locked, vch.Prepay)
	}
	return balance, locked, nil
}

func (ch *VirtualChannelMeta) GetKnownResolve() *payments.VirtualChannelState {
	if ch.LastKnownResolve == nil {
		return nil
	}

	cll, err := cell.FromBOC(ch.LastKnownResolve)
	if err != nil {
		return nil
	}

	var st payments.VirtualChannelState
	if err = tlb.LoadFromCell(&st, cll.BeginParse()); err != nil {
		return nil
	}

	if !st.Verify(ch.Key) {
		return nil
	}
	return &st
}

func (ch *VirtualChannelMeta) AddKnownResolve(state *payments.VirtualChannelState) error {
	if !state.Verify(ch.Key) {
		return fmt.Errorf("incorrect signature")
	}

	if ch.LastKnownResolve != nil {
		cl, err := cell.FromBOC(ch.LastKnownResolve)
		if err != nil {
			return err
		}

		var oldState payments.VirtualChannelState
		if err = tlb.LoadFromCell(&oldState, cl.BeginParse()); err != nil {
			return fmt.Errorf("failed to parse old start: %w", err)
		}

		if oldState.Amount.Cmp(state.Amount) > 0 {
			return ErrNewerStateIsKnown
		}
	}

	cl, err := tlb.ToCell(state)
	if err != nil {
		return err
	}

	ch.LastKnownResolve = cl.ToBOC()
	return nil
}

func (h *ChannelHistoryItem) ParseData() any {
	var dst any

	switch h.Action {
	case ChannelHistoryActionTopup,
		ChannelHistoryActionTopupCapacity,
		ChannelHistoryActionWithdraw,
		ChannelHistoryActionWithdrawCapacity:
		dst = &ChannelHistoryActionAmountData{}
	case ChannelHistoryActionTransferIn:
		dst = &ChannelHistoryActionTransferInData{}
	case ChannelHistoryActionTransferOut:
		dst = &ChannelHistoryActionTransferOutData{}
	default:
		return nil
	}

	if err := json.Unmarshal(h.Data, dst); err != nil {
		return nil
	}
	return dst
}
