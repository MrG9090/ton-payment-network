package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"net/http"
	"time"
)

type NodeChain struct {
	Key                string `json:"key"`
	Fee                string `json:"fee"`
	DeadlineGapSeconds int64  `json:"deadline_gap_seconds"`
}

type VirtualSide struct {
	ChannelAddress          string    `json:"channel_address"`
	Capacity                string    `json:"capacity"`
	Fee                     string    `json:"fee"`
	UncooperativeDeadlineAt time.Time `json:"uncooperative_deadline_at"`
	SafeDeadlineAt          time.Time `json:"safe_deadline_at"`
}

type VirtualChannel struct {
	Key       string       `json:"key"`
	Status    string       `json:"status"`
	Amount    string       `json:"amount"`
	Outgoing  *VirtualSide `json:"outgoing"`
	Incoming  *VirtualSide `json:"incoming"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

func (s *Server) handleVirtualGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var key ed25519.PublicKey
	if q := r.URL.Query().Get("key"); q != "" {
		key, err = parseKey(q)
		if err != nil {
			writeErr(w, 400, "incorrect key format: "+err.Error())
			return
		}
	} else {
		writeErr(w, 400, "channel address is not passed")
	}

	meta, err := s.svc.GetVirtualChannelMeta(r.Context(), key)
	if err != nil {
		writeErr(w, 500, "failed to get virtual channel meta: "+err.Error())
		return
	}

	var addr string
	if meta.Outgoing != nil {
		addr = meta.Outgoing.ChannelAddress
	} else if meta.Incoming != nil {
		addr = meta.Incoming.ChannelAddress
	} else {
		writeErr(w, 400, "channel address is unknown")
		return
	}

	ch, err := s.svc.GetChannel(r.Context(), addr)
	if err != nil {
		writeErr(w, 500, "failed to get channel: "+err.Error())
		return
	}

	cc, err := s.svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, false)
	if err != nil {
		writeErr(w, 400, "failed to resolve coin config"+err.Error())
		return
	}

	res, err := s.getVirtual(r.Context(), meta, int(cc.Decimals))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	writeResp(w, res)
}

func (s *Server) handleVirtualList(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var addr *address.Address
	if q := r.URL.Query().Get("address"); q != "" {
		addr, err = address.ParseAddr(q)
		if err != nil {
			writeErr(w, 400, "incorrect address format: "+err.Error())
			return
		}
	} else {
		writeErr(w, 400, "channel address is not passed")
		return
	}

	ch, err := s.svc.GetChannel(r.Context(), addr.String())
	if err != nil {
		writeErr(w, 500, "failed to get channel: "+err.Error())
		return
	}

	cc, err := s.svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, false)
	if err != nil {
		writeErr(w, 400, "failed to resolve coin config"+err.Error())
		return
	}

	var our, their = make([]*VirtualChannel, 0), make([]*VirtualChannel, 0)

	allTheir, err := ch.Their.Conditionals.LoadAll()
	if err != nil {
		writeErr(w, 500, "failed to load their conditionals: "+err.Error())
		return
	}

	for _, kv := range allTheir {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			continue
		}

		meta, err := s.svc.GetVirtualChannelMeta(r.Context(), vch.Key)
		if err != nil {
			writeErr(w, 500, "failed to get virtual channel meta: "+err.Error())
			return
		}

		res, err := s.getVirtual(r.Context(), meta, int(cc.Decimals))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		their = append(their, res)
	}

	allOur, err := ch.Our.Conditionals.LoadAll()
	if err != nil {
		writeErr(w, 500, "failed to load our conditionals: "+err.Error())
		return
	}

	for _, kv := range allOur {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			continue
		}

		meta, err := s.svc.GetVirtualChannelMeta(r.Context(), vch.Key)
		if err != nil {
			writeErr(w, 500, "failed to get virtual channel meta: "+err.Error())
			return
		}

		res, err := s.getVirtual(r.Context(), meta, int(cc.Decimals))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		our = append(our, res)
	}

	writeResp(w, struct {
		Their []*VirtualChannel `json:"their"`
		Our   []*VirtualChannel `json:"our"`
	}{their, our})
}

func (s *Server) getVirtual(ctx context.Context, meta *db.VirtualChannelMeta, decimals int) (*VirtualChannel, error) {
	var status string
	switch meta.Status {
	case db.VirtualChannelStateActive:
		status = "active"
	case db.VirtualChannelStateClosed:
		status = "closed"
	case db.VirtualChannelStateRemoved:
		status = "removed"
	case db.VirtualChannelStateWantRemove:
		status = "want_remove"
	case db.VirtualChannelStateWantClose:
		status = "want_close"
	default:
		return nil, fmt.Errorf("unknown virtual channel %s state: %d", base64.StdEncoding.EncodeToString(meta.Key), meta.Status)
	}

	res := &VirtualChannel{
		Key:       base64.StdEncoding.EncodeToString(meta.Key),
		Status:    status,
		CreatedAt: meta.CreatedAt,
		UpdatedAt: meta.UpdatedAt,
		Amount:    "0",
	}

	if len(meta.LastKnownResolve) > 0 {
		cll, err := cell.FromBOC(meta.LastKnownResolve)
		if err != nil {
			return nil, fmt.Errorf("failed to parse last known resolve BoC: %w", err)
		}

		var st payments.VirtualChannelState
		if err = tlb.LoadFromCell(&st, cll.BeginParse()); err != nil {
			return nil, fmt.Errorf("failed to parse last known resolve state: %w", err)
		}

		if !st.Verify(meta.Key) {
			return nil, fmt.Errorf("failed to verify last known resolve state: %w", err)
		}

		res.Amount = tlb.MustFromNano(st.Amount, decimals).String()
	}

	if meta.Status != db.VirtualChannelStateClosed && meta.Status != db.VirtualChannelStateRemoved {
		if meta.Incoming != nil {
			res.Incoming = &VirtualSide{
				ChannelAddress:          meta.Incoming.ChannelAddress,
				Capacity:                meta.Incoming.Capacity,
				Fee:                     meta.Incoming.Fee,
				UncooperativeDeadlineAt: meta.Incoming.UncooperativeDeadline,
				SafeDeadlineAt:          meta.Incoming.SafeDeadline,
			}
		}

		if meta.Outgoing != nil {
			res.Outgoing = &VirtualSide{
				ChannelAddress:          meta.Outgoing.ChannelAddress,
				Capacity:                meta.Outgoing.Capacity,
				Fee:                     meta.Outgoing.Fee,
				UncooperativeDeadlineAt: meta.Outgoing.UncooperativeDeadline,
				SafeDeadlineAt:          meta.Outgoing.SafeDeadline,
			}
		}
	}

	return res, nil
}

func (s *Server) handleVirtualState(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Key   string `json:"key"`
		State string `json:"state"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	key, err := parseKey(req.Key)
	if err != nil {
		writeErr(w, 400, "failed to parse key: "+err.Error())
		return
	}

	st, err := parseState(req.State, key)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	if err = s.svc.AddVirtualChannelResolve(r.Context(), key, st); err != nil && !errors.Is(err, db.ErrNewerStateIsKnown) {
		writeErr(w, 500, "failed to add virtual channel state: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleVirtualClose(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Key   string `json:"key"`
		State string `json:"state"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	key, err := parseKey(req.Key)
	if err != nil {
		writeErr(w, 400, "failed to parse key: "+err.Error())
		return
	}

	st, err := parseState(req.State, key)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	if err = s.svc.AddVirtualChannelResolve(r.Context(), key, st); err != nil && !errors.Is(err, db.ErrNewerStateIsKnown) {
		writeErr(w, 500, "failed to add virtual channel state: "+err.Error())
		return
	}

	if err = s.svc.CloseVirtualChannel(r.Context(), key); err != nil {
		writeErr(w, 500, "failed to close virtual channel: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleVirtualOpen(w http.ResponseWriter, r *http.Request) {
	type request struct {
		TTLSeconds      int64       `json:"ttl_seconds"`
		Capacity        string      `json:"capacity"`
		JettonMaster    string      `json:"jetton_master"`
		ExtraCurrencyID uint32      `json:"ec_id"`
		NodesChain      []NodeChain `json:"nodes_chain"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	var jetton *address.Address
	if req.JettonMaster != "" {
		var err error
		jetton, err = address.ParseAddr(req.JettonMaster)
		if err != nil {
			writeErr(w, 400, "incorrect jetton address format: "+err.Error())
			return
		}

		if req.ExtraCurrencyID != 0 {
			writeErr(w, 400, "jetton master address and extra currency id are mutually exclusive")
			return
		}
	}

	if len(req.NodesChain) == 0 {
		writeErr(w, 400, "no nodes passed")
		return
	}

	deadline := time.Now().Add(time.Duration(req.TTLSeconds) * time.Second)

	deadlines := make([]time.Time, len(req.NodesChain))
	for i := range req.NodesChain {
		deadlines[i] = deadline
		deadline = deadline.Add(time.Duration(req.NodesChain[i].DeadlineGapSeconds) * time.Second)
	}

	cc, err := s.svc.ResolveCoinConfig(req.JettonMaster, req.ExtraCurrencyID, true)
	if err != nil {
		writeErr(w, 400, "failed to resolve coin config"+err.Error())
		return
	}

	capacity, err := tlb.FromDecimal(req.Capacity, int(cc.Decimals))
	if err != nil {
		writeErr(w, 400, "failed to parse capacity: "+err.Error())
		return
	}

	var with []byte
	var tunChain []transport.TunnelChainPart
	for i, node := range req.NodesChain {
		key, err := parseKey(node.Key)
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" key: "+err.Error())
			return
		}

		fee, err := tlb.FromDecimal(node.Fee, int(cc.Decimals))
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" fee: "+err.Error())
			return
		}

		if with == nil {
			with = key
		}

		tunChain = append(tunChain, transport.TunnelChainPart{
			Target:   key,
			Capacity: capacity.Nano(),
			Fee:      fee.Nano(),
			Deadline: deadlines[i],
		})
	}

	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		writeErr(w, 500, "failed to generate key: "+err.Error())
		return
	}

	vc, firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 5, false, s.svc.GetPrivateKey())
	if err != nil {
		writeErr(w, 500, "failed to generate tunnel: "+err.Error())
		return
	}

	err = s.svc.OpenVirtualChannel(r.Context(), with, firstInstructionKey, tunChain[len(tunChain)-1].Target, vPriv, tun, vc, jetton, req.ExtraCurrencyID)
	if err != nil {
		writeErr(w, 403, "failed to request virtual channel open: "+err.Error())
		return
	}

	writeResp(w, struct {
		PublicKey      string    `json:"public_key"`
		PrivateKeySeed string    `json:"private_key_seed"`
		Status         string    `json:"status"`
		Deadline       time.Time `json:"deadline"`
	}{
		PublicKey:      base64.StdEncoding.EncodeToString(vPriv.Public().(ed25519.PublicKey)),
		PrivateKeySeed: base64.StdEncoding.EncodeToString(vPriv.Seed()),
		Status:         "pending",
		Deadline:       deadlines[len(req.NodesChain)-1],
	})
}

func (s *Server) handleVirtualTransfer(w http.ResponseWriter, r *http.Request) {
	type request struct {
		TTLSeconds      int64       `json:"ttl_seconds"`
		Amount          string      `json:"amount"`
		JettonMaster    string      `json:"jetton_master"`
		ExtraCurrencyID uint32      `json:"ec_id"`
		NodesChain      []NodeChain `json:"nodes_chain"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	var jetton *address.Address
	if req.JettonMaster != "" {
		var err error
		jetton, err = address.ParseAddr(req.JettonMaster)
		if err != nil {
			writeErr(w, 400, "incorrect jetton address format: "+err.Error())
			return
		}

		if req.ExtraCurrencyID != 0 {
			writeErr(w, 400, "jetton master address and extra currency id are mutually exclusive")
			return
		}
	}

	if len(req.NodesChain) == 0 {
		writeErr(w, 400, "no nodes passed")
		return
	}

	cc, err := s.svc.ResolveCoinConfig(req.JettonMaster, req.ExtraCurrencyID, false)
	if err != nil {
		writeErr(w, 400, "failed to resolve coin config"+err.Error())
		return
	}

	deadline := time.Now().Add(time.Duration(req.TTLSeconds) * time.Second)

	deadlines := make([]time.Time, len(req.NodesChain))
	for i := range req.NodesChain {
		deadlines[i] = deadline
		deadline = deadline.Add(time.Duration(req.NodesChain[i].DeadlineGapSeconds) * time.Second)
	}

	capacity, err := tlb.FromDecimal(req.Amount, int(cc.Decimals))
	if err != nil {
		writeErr(w, 400, "failed to parse capacity: "+err.Error())
		return
	}

	var with []byte
	var tunChain []transport.TunnelChainPart
	for i, node := range req.NodesChain {
		key, err := parseKey(node.Key)
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" key: "+err.Error())
			return
		}

		fee, err := tlb.FromDecimal(node.Fee, int(cc.Decimals))
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" fee: "+err.Error())
			return
		}

		if with == nil {
			with = key
		}

		tunChain = append(tunChain, transport.TunnelChainPart{
			Target:   key,
			Capacity: capacity.Nano(),
			Fee:      fee.Nano(),
			Deadline: deadlines[i],
		})
	}

	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		writeErr(w, 500, "failed to generate key: "+err.Error())
		return
	}

	vc, firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 5, true, s.svc.GetPrivateKey())
	if err != nil {
		writeErr(w, 500, "failed to generate tunnel: "+err.Error())
		return
	}

	err = s.svc.OpenVirtualChannel(r.Context(), with, firstInstructionKey, tunChain[len(tunChain)-1].Target, vPriv, tun, vc, jetton, req.ExtraCurrencyID)
	if err != nil {
		writeErr(w, 403, "failed to request virtual channel open: "+err.Error())
		return
	}

	writeResp(w, struct {
		Status   string    `json:"status"`
		Deadline time.Time `json:"deadline"`
	}{
		Status:   "pending",
		Deadline: deadlines[len(req.NodesChain)-1],
	})
}

func (s *Server) PushVirtualChannelEvent(ctx context.Context, event db.VirtualChannelEventType, meta *db.VirtualChannelMeta, cc *config.CoinConfig) error {
	vc, err := s.getVirtual(ctx, meta, int(cc.Decimals))
	if err != nil {
		return fmt.Errorf("failed to get virtual channel: %w", err)
	}

	if err := s.queue.CreateTask(ctx, WebhooksTaskPool, "virtual-channel-event", "events",
		vc.Key+"-"+string(event)+"-"+fmt.Sprint(meta.UpdatedAt),
		db.VirtualChannelEvent{
			EventType:      event,
			VirtualChannel: vc,
		}, nil, nil,
	); err != nil {
		return fmt.Errorf("failed to create virtual-channel-event task: %w", err)
	}
	return nil
}
