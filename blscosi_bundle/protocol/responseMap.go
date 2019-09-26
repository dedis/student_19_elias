package protocol

import (
	"errors"
	"sort"

	"go.dedis.ch/kyber/v3"
	"go.dedis.ch/kyber/v3/pairing"
	"go.dedis.ch/kyber/v3/sign"
	"go.dedis.ch/kyber/v3/sign/bdn"
	"go.dedis.ch/kyber/v3/sign/bls"
	"go.dedis.ch/onet/v3/log"
)

// Responses is the container used to store responses coming from the children
type Responses interface {
	Add(idx int, r *Response) error
	Update(map[uint32](*Response)) error
	Count() int
	// Aggregate aggregates all the signatures in responses.
	// Also aggregates the bitmasks.
	Aggregate(suite pairing.Suite, publics []kyber.Point) (kyber.Point, *sign.Mask, error)
	// The underlying map
	Map() map[uint32](*Response)
}

type SimpleResponses map[uint32]*Response

func (responses SimpleResponses) Add(idx int, response *Response) error {
	responses[uint32(idx)] = response
	return nil
}

func (responses SimpleResponses) Update(newResponses map[uint32](*Response)) error {
	for key, response := range newResponses {
		responses[key] = response
	}
	return nil
}

func (responses SimpleResponses) Count() int {
	return len(responses)
}

func (responses SimpleResponses) Aggregate(suite pairing.Suite, publics []kyber.Point) (
	kyber.Point, *sign.Mask, error) {

	var sigs [][]byte
	aggMask, err := sign.NewMask(suite, publics, nil)
	if err != nil {
		return nil, nil, err
	}

	log.Lvlf3("aggregating total of %d signatures", aggMask.CountEnabled())

	var keys []uint32
	for k := range responses {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		res := responses[k]
		sigs = append(sigs, res.Signature)
		err := aggMask.Merge(res.Mask)
		if err != nil {
			return nil, nil, err
		}
	}

	aggSig, err := bdn.AggregateSignatures(suite, sigs, aggMask)
	if err != nil {
		return nil, nil, err
	}

	return aggSig, aggMask, err
}

func (responses SimpleResponses) Map() map[uint32](*Response) {
	return responses
}

type TreeResponses struct {
	responses map[uint32]*Response
	total     int // number of nodes
	mask      *sign.Mask
	tree      map[uint32][]uint32
	parents   map[uint32]uint32
	publics   []kyber.Point
	suite     pairing.Suite
}

func NewTreeResponses(suite pairing.Suite, publics []kyber.Point) (TreeResponses, error) {
	mask, err := sign.NewMask(suite, publics, nil)
	if err != nil {
		return TreeResponses{}, err
	}

	tree := make(map[uint32][]uint32)
	parents := make(map[uint32]uint32)

	start := uint32(1)
	for start < uint32(len(publics)) {
		start *= 2
	}
	startBelow := uint32(0)
	endBelow := uint32(len(publics))

	// Go up the tree twoards the root
	// size is the maximum capacity of a level
	for size := start / 2; size > 0; size /= 2 {
		i := start
		for left := startBelow; left < endBelow; left += 2 {
			right := left + 1
			if right < endBelow {
				tree[i] = []uint32{left, right}
				parents[right] = i
			} else {
				tree[i] = []uint32{left}
			}
			parents[left] = i
			i++
		}

		startBelow = start
		endBelow = i
		start += size
	}

	log.Lvl5("Tree:", tree)

	return TreeResponses{
		responses: make(map[uint32]*Response),
		total:     len(publics),
		mask:      mask,
		tree:      tree,
		parents:   parents,
		publics:   publics,
		suite:     suite,
	}, nil
}

func (treeRes TreeResponses) Add(idx int, r *Response) error {
	// It is best that we multiply each signature with its coefficient immediately

	mask, err := sign.NewMask(treeRes.suite, treeRes.publics, nil)
	if err != nil {
		return err
	}

	err = mask.Merge(r.Mask)
	if err != nil {
		return err
	}

	sig := [][]byte{r.Signature}
	aggSig, err := bdn.AggregateSignatures(treeRes.suite, sig, mask)
	if err != nil {
		return err
	}
	data, err := aggSig.MarshalBinary()
	if err != nil {
		return err
	}
	return treeRes.addAggregated(uint32(idx), data, mask)
}

func (treeRes TreeResponses) addAggregated(idx uint32, sig []byte, mask *sign.Mask) error {
	log.Lvl5("Before:", treeRes.responses)
	log.Lvl5("Adding to tree:", idx)

	for current := idx; ; {
		_, ok := treeRes.responses[current]
		if ok {
			return nil
		}
		current, ok = treeRes.parents[current]
		if !ok {
			break
		}
	}

	var children []uint32
	parent, ok := treeRes.parents[idx]
	aggregate := ok
	if !ok {
		children = []uint32{idx} // root
	} else {
		children, ok = treeRes.tree[parent]
		if !ok {
			return errors.New("Node not in tree")
		}
	}

	var childSigs [][]byte
	for _, child := range children {
		r, ok := treeRes.responses[child]
		if ok {
			childSigs = append(childSigs, r.Signature)
		}
		if !ok && child != idx {
			aggregate = false // nothing to aggregate
			break
		}
	}

	if aggregate {
		childSigs = append(childSigs, sig)
		aggSig, err := bls.AggregateSignatures(treeRes.suite, childSigs...)
		if err != nil {
			return err
		}

		for _, child := range children {
			r, ok := treeRes.responses[child]
			if ok {
				err = mask.Merge(r.Mask)
				if err != nil {
					return err
				}
			}
		}

		err = treeRes.addAggregated(parent, aggSig, mask)
		if err != nil {
			return err
		}
	} else {
		treeRes.responses[idx] = &Response{sig, mask.Mask()}
		err := treeRes.mask.Merge(mask.Mask())
		if err != nil {
			return err
		}

		// delete all nodes below
		var d []uint32 // descendents, stack
		d = append(d, treeRes.tree[idx]...)
		i := 0
		for len(d) > 0 {
			var desc uint32
			d, desc = d[:len(d)-1], d[len(d)-1]
			delete(treeRes.responses, desc)
			d = append(d, treeRes.tree[desc]...)
			i++
		}
	}

	log.Lvl5("After:", treeRes.responses)

	return nil
}

func (treeRes TreeResponses) Update(newResponses map[uint32](*Response)) error {
	for k, resp := range newResponses {
		mask, err := sign.NewMask(treeRes.suite, treeRes.publics, nil)
		if err != nil {
			return err
		}

		err = mask.Merge(resp.Mask)
		if err != nil {
			return err
		}

		err = treeRes.addAggregated(k, resp.Signature, mask)
		if err != nil {
			return err
		}
	}
	return nil
}

func (treeRes TreeResponses) Count() int {
	return treeRes.mask.CountEnabled()
}

func (treeRes TreeResponses) Aggregate(suite pairing.Suite, publics []kyber.Point) (
	kyber.Point, *sign.Mask, error) {

	var sigs [][]byte
	for _, sig := range treeRes.responses {
		sigs = append(sigs, sig.Signature)
	}

	// These signatures have already been multiplied with their coefficients
	// So we use the plain BLS aggregation rather than BDN
	sig, err := bls.AggregateSignatures(suite, sigs...)
	if err != nil {
		return nil, nil, err
	}

	asPoint := suite.G1().Point()
	err = asPoint.UnmarshalBinary(sig)
	if err != nil {
		return nil, nil, err
	}
	return asPoint, treeRes.mask, nil
}

func (treeRes TreeResponses) Map() map[uint32](*Response) {
	return treeRes.responses
}
