// Copyright © 2016 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package router

import (
	"github.com/KtorZ/rpc/core"
	"github.com/TheThingsNetwork/ttn/core/dutycycle"
	"github.com/TheThingsNetwork/ttn/utils/errors"
	"github.com/TheThingsNetwork/ttn/utils/stats"
	"github.com/apex/log"
	"golang.org/x/net/context"
)

type component struct {
	Storage
	manager dutycycle.DutyManager
	brokers []core.BrokerClient
	ctx     log.Interface
}

// New constructs a new router
func New(db Storage, dm dutycycle.DutyManager, brokers []core.BrokerClient, ctx log.Interface) core.RouterServer {
	return component{Storage: db, manager: dm, brokers: brokers, ctx: ctx}
}

// HandleStats implements the core.RouterClient interface
func (r component) HandleStats(ctx context.Context, req *core.StatsReq) (*core.StatsRes, error) {
	if req == nil {
		return nil, errors.New(errors.Structural, "Invalid nil stats request")
	}

	if len(req.GatewayID) != 8 {
		return nil, errors.New(errors.Structural, "Invalid gateway identifier")
	}

	if req.Metadata == nil {
		return nil, errors.New(errors.Structural, "Missing mandatory Metadata")
	}

	stats.MarkMeter("router.stat.in")
	return nil, r.UpdateStats(req.GatewayID, *req.Metadata)
}

// HandleData implements the core.RouterClient interface
func (r component) HandleData(ctx context.Context, req *core.DataRouterReq) (*core.DataRouterRes, error) {
	// Get some logs / analytics
	r.ctx.Debug("Handling uplink packet")
	stats.MarkMeter("router.uplink.in")

	// Validate coming data
	_, _, fhdr, _, err := core.ValidateLoRaWANData(req.Payload)
	if err != nil {
		return nil, errors.New(errors.Structural, err)
	}
	if req.Metadata == nil {
		return nil, errors.New(errors.Structural, "Missing mandatory Metadata")
	}
	if len(req.GatewayID) != 8 {
		return nil, errors.New(errors.Structural, "Invalid gatewayID")
	}

	// Lookup for an existing broker
	entries, err := r.Lookup(fhdr.DevAddr)
	if err != nil && err.(errors.Failure).Nature != errors.NotFound {
		r.ctx.Warn("Database lookup failed")
		return nil, errors.New(errors.Operational, err)
	}
	shouldBroadcast := err != nil

	// Add Gateway location metadata
	if gmeta, err := r.LookupStats(req.GatewayID); err == nil {
		req.Metadata.Latitude = gmeta.Latitude
		req.Metadata.Longitude = gmeta.Longitude
		req.Metadata.Altitude = gmeta.Altitude
	}

	// Add Gateway duty metadata
	cycles, err := r.manager.Lookup(req.GatewayID)
	if err != nil {
		r.ctx.WithError(err).Debug("Unable to get any metadata about duty-cycles")
		cycles = make(dutycycle.Cycles)
	}

	sb1, err := dutycycle.GetSubBand(float64(req.Metadata.Frequency))
	if err != nil {
		stats.MarkMeter("router.uplink.not_supported")
		return nil, errors.New(errors.Structural, "Unhandled uplink signal frequency")
	}

	rx1, rx2 := uint(dutycycle.StateFromDuty(cycles[sb1])), uint(dutycycle.StateFromDuty(cycles[dutycycle.EuropeG3]))
	req.Metadata.DutyRX1, req.Metadata.DutyRX2 = uint32(rx1), uint32(rx2)

	bpacket := &core.DataBrokerReq{Payload: req.Payload, Metadata: req.Metadata}

	// Send packet to broker(s)
	var response *core.DataBrokerRes
	if shouldBroadcast {
		// No Recipient available -> broadcast
		response, err = r.send(bpacket, r.brokers...)
	} else {
		// Recipients are available
		var brokers []core.BrokerClient
		for _, e := range entries {
			brokers = append(brokers, r.brokers[e.BrokerIndex])
		}
		response, err = r.send(bpacket, brokers...)
		if err != nil && err.(errors.Failure).Nature == errors.NotFound {
			// Might be a collision with the dev addr, we better broadcast
			response, err = r.send(bpacket, r.brokers...)
		}
		stats.MarkMeter("router.uplink.out")
	}

	if err != nil {
		switch err.(errors.Failure).Nature {
		case errors.NotFound:
			stats.MarkMeter("router.uplink.negative_broker_response")
		default:
			stats.MarkMeter("router.uplink.bad_broker_response")
		}
		return nil, err
	}

	return r.handleDataDown(response, req.GatewayID)
}

func (r component) handleDataDown(req *core.DataBrokerRes, gatewayID []byte) (*core.DataRouterRes, error) {
	if req == nil { // No response
		return nil, nil
	}

	// Update downlink metadata for the related gateway
	if req.Metadata == nil {
		stats.MarkMeter("router.uplink.bad_broker_response")
		return nil, errors.New(errors.Structural, "Missing mandatory Metadata in response")
	}
	freq := float64(req.Metadata.Frequency)
	datr := req.Metadata.DataRate
	codr := req.Metadata.CodingRate
	size := uint(req.Metadata.PayloadSize)
	if err := r.manager.Update(gatewayID, freq, size, datr, codr); err != nil {
		return nil, errors.New(errors.Operational, err)
	}

	// Send Back the response
	return &core.DataRouterRes{Payload: req.Payload, Metadata: req.Metadata}, nil
}

func (r component) send(req *core.DataBrokerReq, brokers ...core.BrokerClient) (*core.DataBrokerRes, error) {

	return nil, nil
}

// Register implements the core.Router interface
//func (r component) Register(reg Registration, an AckNacker) (err error) {
//	defer ensureAckNack(an, nil, &err)
//	stats.MarkMeter("router.registration.in")
//	r.ctx.Debug("Handling registration")
//
//	rreg, ok := reg.(RRegistration)
//	if !ok {
//		err = errors.New(errors.Structural, "Unexpected registration type")
//		r.ctx.WithError(err).Warn("Unable to register")
//		return err
//	}
//
//	return r.Store(rreg)
//}
// handleDataDown controls that data received from an uplink are okay.
// It also updates metadata about the related gateway
