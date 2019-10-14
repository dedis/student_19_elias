// Package blscosi_substract implements a service and client that provides an API
// to request a signature to a cothority
package blscosi_substract

import (
	"errors"
	"time"

	"github.com/dedis/student_19_elias/blscosi_substract/protocol"
	"go.dedis.ch/kyber/v3/pairing"
	"go.dedis.ch/kyber/v3/suites"
	"go.dedis.ch/onet/v3"
	"go.dedis.ch/onet/v3/log"
	"go.dedis.ch/onet/v3/network"
)

const protocolTimeout = 20 * time.Second

var suite = suites.MustFind("bn256.adapter").(*pairing.SuiteBn256)

// ServiceID is the key to get the service later
var ServiceID onet.ServiceID

// ServiceName is the name to refer to the CoSi service
const ServiceName = "substractCoSiService"

func init() {
	ServiceID, _ = onet.RegisterNewServiceWithSuite(ServiceName, suite, newCoSiService)
	network.RegisterMessage(&SignatureRequest{})
	network.RegisterMessage(&SignatureResponse{})
}

// Service is the service that handles collective signing operations
type Service struct {
	*onet.ServiceProcessor
	suite     pairing.Suite
	Threshold int
	Timeout   time.Duration
}

// SignatureRequest is what the Cosi service is expected to receive from clients.
type SignatureRequest struct {
	Message []byte
	Roster  *onet.Roster
	Params  protocol.Parameters
}

// SignatureResponse is what the Cosi service will reply to clients.
type SignatureResponse struct {
	Hash      []byte
	Signature protocol.BlsSignature
}

// SignatureRequest treats external request to this service.
func (s *Service) SignatureRequest(req *SignatureRequest) (network.Message, error) {
	// generate the tree
	rooted := req.Roster.NewRosterWithRoot(s.ServerIdentity())
	if rooted == nil {
		return nil, errors.New("we're not in the roster")
	}
	tree := rooted.GenerateStar()
	if tree == nil {
		return nil, errors.New("failed to generate tree")
	}

	// configure the BlsCosiSubstract protocol
	pi, err := s.CreateProtocol(protocol.DefaultProtocolName, tree)
	if err != nil {
		return nil, errors.New("Couldn't make new protocol: " + err.Error())
	}
	p := pi.(*protocol.BlsCosiSubstract)
	p.Timeout = s.Timeout
	p.Msg = req.Message
	p.Params = req.Params
	if p.Params == (protocol.Parameters{}) {
		p.Params = protocol.DefaultParams()
	}

	// Threshold before the subtrees so that we can optimize situation
	// like a threshold of one
	if s.Threshold > 0 {
		p.Threshold = s.Threshold
	}

	// start the protocol
	log.Lvl3("CoSi service starting up gossip protocol")
	if err = pi.Start(); err != nil {
		return nil, err
	}

	// wait for reply. This will always eventually return.
	sig := <-p.FinalSignature

	// The hash is the message blscosi actually signs, we recompute it the
	// same way as blscosi and then return it.
	h := s.suite.Hash()
	h.Write(req.Message)
	return &SignatureResponse{h.Sum(nil), sig}, nil
}

// NewProtocol is called on all nodes of a Tree (except the root, since it is
// the one starting the protocol) so it's the Service that will be called to
// generate the PI on all others node.
func (s *Service) NewProtocol(tn *onet.TreeNodeInstance, conf *onet.GenericConfig) (onet.ProtocolInstance, error) {
	log.Lvl3("Cosi Service received on", s.ServerIdentity(), "received new protocol event-", tn.ProtocolName())
	if tn.ProtocolName() != protocol.DefaultProtocolName {
		return nil, errors.New("no such protocol " + tn.ProtocolName())
	}
	return protocol.NewDefaultProtocol(tn)
}

func newCoSiService(c *onet.Context) (onet.Service, error) {
	s := &Service{
		ServiceProcessor: onet.NewServiceProcessor(c),
		suite:            suite,
		Timeout:          protocolTimeout,
	}

	if err := s.RegisterHandler(s.SignatureRequest); err != nil {
		log.Error("couldn't register message:", err)
		return nil, err
	}

	return s, nil
}
