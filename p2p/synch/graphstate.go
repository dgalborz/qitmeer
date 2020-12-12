/*
 * Copyright (c) 2017-2020 The qitmeer developers
 */

package synch

import (
	"context"
	"errors"
	"fmt"
	"github.com/Qitmeer/qitmeer/p2p/common"
	"github.com/Qitmeer/qitmeer/p2p/peers"
	pb "github.com/Qitmeer/qitmeer/p2p/proto/v1"
	libp2pcore "github.com/libp2p/go-libp2p-core"
	"sync/atomic"
)

func (s *Sync) sendGraphStateRequest(ctx context.Context, pe *peers.Peer, gs *pb.GraphState) (*pb.GraphState, error) {
	ctx, cancel := context.WithTimeout(ctx, ReqTimeout)
	defer cancel()

	stream, err := s.Send(ctx, gs, RPCGraphState, pe.GetID())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := stream.Reset(); err != nil {
			log.Error(fmt.Sprintf("Failed to reset stream with protocol %s,%v", stream.Protocol(), err))
		}
	}()

	code, errMsg, err := ReadRspCode(stream, s.Encoding())
	if err != nil {
		return nil, err
	}

	if code != ResponseCodeSuccess {
		s.Peers().IncrementBadResponses(stream.Conn().RemotePeer())
		return nil, errors.New(errMsg)
	}

	msg := &pb.GraphState{}
	if err := s.Encoding().DecodeWithMaxLength(stream, msg); err != nil {
		return nil, err
	}

	return msg, err
}

func (s *Sync) graphStateHandler(ctx context.Context, msg interface{}, stream libp2pcore.Stream) *common.P2PError {
	pe := s.peers.Get(stream.Conn().RemotePeer())
	if pe == nil {
		return common.NewP2PError(common.ErrPeerUnknown, peers.ErrPeerUnknown)
	}

	ctx, cancel := context.WithTimeout(ctx, HandleTimeout)
	var err error
	respCode := ResponseCodeServerError
	defer func() {
		if respCode != ResponseCodeSuccess {
			resp, err := s.generateErrorResponse(respCode, err.Error())
			if err != nil {
				log.Error(fmt.Sprintf("Failed to generate a response error:%v", err))
			} else {
				if _, err := stream.Write(resp); err != nil {
					log.Debug(fmt.Sprintf("Failed to write to stream:%v", err))
				}
			}
		}
		cancel()
	}()

	m, ok := msg.(*pb.GraphState)
	if !ok {
		err = fmt.Errorf("message is not type *pb.GraphState")
		return common.NewP2PError(common.ErrMessage, err)
	}
	pe.UpdateGraphState(m)
	go s.peerSync.PeerUpdate(pe, false)

	e := s.EncodeResponseMsg(stream, s.getGraphState())
	if e != nil {
		return e
	}
	respCode = ResponseCodeSuccess
	return nil
}

func (ps *PeerSync) processUpdateGraphState(pe *peers.Peer) error {
	if !pe.IsActive() {
		return fmt.Errorf("peer is not active")
	}
	gs, err := ps.sy.sendGraphStateRequest(ps.sy.p2p.Context(), pe, ps.sy.getGraphState())
	if err != nil {
		log.Error(err.Error())
		return err
	}
	pe.UpdateGraphState(gs)
	go ps.PeerUpdate(pe, false)
	return nil
}

func (ps *PeerSync) UpdateGraphState(pe *peers.Peer) {
	// Ignore if we are shutting down.
	if atomic.LoadInt32(&ps.shutdown) != 0 {
		return
	}

	ps.msgChan <- &UpdateGraphStateMsg{pe: pe}
}
