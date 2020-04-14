package pbft

import (
	"math/rand"
	"testing"

	"github.com/elastos/Elastos.ELA.SideChain.ETH/common"

	"github.com/stretchr/testify/assert"
)

func getRandProducers() []common.Address {
	size := rand.Intn(20) + 5
	signers := make([]common.Address, size)
	for i := 0; i < size; i++ {
		data := make([]byte, len(common.Address{}))
		rand.Read(data)
		signers[i] = common.BytesToAddress(data)
	}
	return signers
}

func TestNewProducers(t *testing.T) {
	signers := getRandProducers()
	p := NewProducers(signers)
	producers := p.GetProducers()
	for i, v := range signers {
		assert.Equal(t, producers[i], v)
	}

	index := rand.Intn(len(signers))
	assert.True(t,  p.IsProducers(&signers[index]))

	data := make([]byte, len(common.Address{}))
	rand.Read(data)
	wrongSigner := common.BytesToAddress(data)
	assert.False(t,  p.IsProducers(&wrongSigner))
}

func TestProducers_IsOnduty(t *testing.T) {
	signers := getRandProducers()
	p := NewProducers(signers)

	changeCount := rand.Intn(200)
	for i := 0; i < changeCount; i++ {
		p.ChangeHeight()
		signer := signers[(i + 1) %len(signers)]
		assert.True(t, p.IsOnduty(&signer))
	}

	assert.Equal(t, p.GetDutyIndex(), changeCount)
}