package redisutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSlot checks Slot against the slots that Redis Cluster reports with
// CLUSTER KEYSLOT, and the hash tag rules of the cluster specification.
func TestSlot(t *testing.T) {
	for key, want := range map[string]int{
		"123456789": 0x31C3 % SlotCount, // the check value of CRC-16/XMODEM
		"foo":       12182,
		"bar":       5061,
		"":          0,
	} {
		assert.Equal(t, want, Slot(key), "%q", key)
	}

	// The part between the first { and the next } decides the slot.
	assert.Equal(t, Slot("user1000"), Slot("{user1000}.following"))
	assert.Equal(t, Slot("{user1000}.following"), Slot("{user1000}.followers"))
	assert.Equal(t, Slot("user1000"), Slot("foo{user1000}{bar}"), "only the first tag counts")
	// An empty tag, or a { without a }, is no tag: the whole key decides.
	assert.Equal(t, crc16("foo{}{bar}")%SlotCount, uint16(Slot("foo{}{bar}")))
	assert.Equal(t, crc16("foo{bar")%SlotCount, uint16(Slot("foo{bar")))
	assert.Equal(t, Slot("{bar"), Slot("foo{{bar}}zap"), "the tag of foo{{bar}}zap is {bar")

	assert.Equal(t, "{abc}", HashTag("abc"))
	assert.Equal(t, Slot("otp:contact:"+HashTag("abc")+":requests"), Slot("otp:contact:"+HashTag("abc")+":client:x:attempts"))
}
