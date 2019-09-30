package main

/*
The simulation-file can be used with the `cothority/simul` and be run either
locally or on deterlab. Contrary to the `test` of the protocol, the simulation
is much more realistic, as it tests the protocol on different nodes, and not
only in a test-environment.

The Setup-method is run once on the client and will create all structures
and slices necessary to the simulation. It also receives a 'dir' argument
of a directory where it can write files. These files will be copied over to
the simulation so that they are available.

The Run-method is called only once by the root-node of the tree defined in
Setup. It should run the simulation in different rounds. It can also
measure the time each run takes.

In the Node-method you can read the files that have been created by the
'Setup'-method.
*/

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/BurntSushi/toml"
	blscosi "github.com/dedis/student_19_elias/blscosi_bundle"
	"github.com/dedis/student_19_elias/blscosi_bundle/protocol"
	"go.dedis.ch/kyber/v3/pairing"
	"go.dedis.ch/onet/v3"
	"go.dedis.ch/onet/v3/log"
	"go.dedis.ch/onet/v3/network"
	"go.dedis.ch/onet/v3/simul/monitor"
)

const roundSleep = 4 * time.Second

func init() {
	onet.SimulationRegister("BlsCosiBundleProtocol", NewSimulationProtocol)
}

// SimulationProtocol implements onet.Simulation.
type SimulationProtocol struct {
	onet.SimulationBFTree
	FailingLeaves int
	MinDelay      float64
	MaxDelay      float64
	GossipTick    float64
	RumorPeers    int
	ShutdownPeers int
	TreeMode      int
}

// NewSimulationProtocol is used internally to register the simulation (see the init()
// function above).
func NewSimulationProtocol(config string) (onet.Simulation, error) {
	es := &SimulationProtocol{}
	_, err := toml.Decode(config, es)
	if err != nil {
		return nil, err
	}
	return es, nil
}

// Setup implements onet.Simulation.
func (s *SimulationProtocol) Setup(dir string, hosts []string) (*onet.SimulationConfig, error) {
	sc := &onet.SimulationConfig{}
	s.CreateRoster(sc, hosts, 2000)
	err := s.CreateTree(sc)
	if err != nil {
		return nil, err
	}

	return sc, nil
}

// Node can be used to initialize each node before it will be run
// by the server. Here we call the 'Node'-method of the
// SimulationBFTree structure which will load the roster- and the
// tree-structure to speed up the first round.
func (s *SimulationProtocol) Node(config *onet.SimulationConfig) error {
	index, _ := config.Roster.Search(config.Server.ServerIdentity.ID)
	if index < 0 {
		log.Fatal("Didn't find this node in roster")
	}

	leaves := config.Tree.Root.Children

	if s.MaxDelay > 0 {
		// delay announcements
		config.Server.RegisterProcessorFunc(onet.ProtocolMsgID, func(e *network.Envelope) error {
			//get message
			_, msg, err := network.Unmarshal(e.Msg.(*onet.ProtocolMsg).MsgSlice, config.Server.Suite())
			if err != nil {
				log.Fatal("error while unmarshaling a message:", err)
				return err
			}

			switch msg.(type) {
			case *protocol.Rumor, *protocol.Shutdown:
				sleepSecs := rand.Float64()*(s.MaxDelay-s.MinDelay) + s.MinDelay
				sleepNsecs := sleepSecs * float64(time.Second/time.Nanosecond)
				log.Lvlf3("Delaying message by %.3f for simulation on %v", sleepSecs, config.Server.ServerIdentity)
				time.Sleep(time.Duration(sleepNsecs))
			}
			config.Overlay.Process(e)
			return nil
		})
	}

	numToIntercept := s.FailingLeaves
	if len(leaves) < s.FailingLeaves {
		log.Lvl1("Warning: not enough children for failing. Is the shape of the tree correct?")
		numToIntercept = len(leaves)
	}
	toIntercept := leaves[:numToIntercept]
	// intercept announcements on some nodes
	for _, n := range toIntercept {
		if n.ServerIdentity.ID.Equal(config.Server.ServerIdentity.ID) {
			// This will override the delay ProcessorFunc, which is fine.
			config.Server.RegisterProcessorFunc(onet.ProtocolMsgID, func(e *network.Envelope) error {
				//get message
				_, msg, err := network.Unmarshal(e.Msg.(*onet.ProtocolMsg).MsgSlice, config.Server.Suite())
				if err != nil {
					log.Fatal("error while unmarshaling a message:", err)
					return err
				}

				switch msg.(type) {
				case *protocol.Rumor, *protocol.Shutdown:
					log.Lvl2("Ignoring blscosi message for simulation on ", config.Server.ServerIdentity)
				default:
					config.Overlay.Process(e)
				}
				return nil
			})
			break // this node has been found
		}
	}

	log.Lvl3("Initializing node-index", index)
	return s.SimulationBFTree.Node(config)
}

// Run implements onet.Simulation.
func (s *SimulationProtocol) Run(config *onet.SimulationConfig) error {
	size := config.Tree.Size()
	log.Lvl2("Size is:", size, "rounds:", s.Rounds)
	for round := 0; round < s.Rounds; round++ {
		log.Lvl1("Starting round", round)

		time.Sleep(roundSleep)

		round := monitor.NewTimeMeasure("round")
		blscosiService := config.GetService(blscosi.ServiceName).(*blscosi.Service)

		blscosiService.Threshold = s.Hosts - (s.Hosts-1)/3

		params := protocol.Parameters{
			GossipTick:    time.Duration(s.GossipTick * float64(time.Second/time.Nanosecond)),
			RumorPeers:    s.RumorPeers,
			ShutdownPeers: s.ShutdownPeers,
			TreeMode:      s.TreeMode != 0,
		}

		client := blscosi.NewClient()
		proposal := []byte{0xFF}
		serviceReq := &blscosi.SignatureRequest{
			Roster:  config.Roster,
			Message: proposal,
			Params:  params,
		}
		serviceReply := &blscosi.SignatureResponse{}

		log.Lvl1("Sending request to service...")
		err := client.SendProtobuf(config.Server.ServerIdentity, serviceReq, serviceReply)
		if err != nil {
			return fmt.Errorf("Cannot send:%s", err)
		}

		round.Record()

		suite := client.Suite().(pairing.Suite)
		publics := config.Roster.ServicePublics(blscosi.ServiceName)

		err = serviceReply.Signature.VerifyAggregate(suite, proposal, publics)
		if err != nil {
			return fmt.Errorf("error while verifying signature:%s", err)
		}

		mask, err := serviceReply.Signature.GetMask(suite, publics)
		monitor.RecordSingleMeasure("correct_nodes", float64(mask.CountEnabled()))

		log.Lvl2("Signature correctly verified!")
	}
	return nil
}
