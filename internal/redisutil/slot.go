package redisutil

import "strings"

// SlotCount is the number of hash slots of Redis Cluster.
const SlotCount = 16384

// Slot returns the hash slot of key, as Redis Cluster computes it: the
// CRC16 of the key, or of its hash tag, modulo 16384. The hash tag is the
// part between the first "{" and the next "}", when that part is not
// empty. A Lua script or a MULTI may only touch keys of one slot in Redis
// Cluster, so the stores put the keys that one script uses under one hash
// tag, and their tests check it with Slot.
func Slot(key string) int {
	if start := strings.IndexByte(key, '{'); start >= 0 {
		if end := strings.IndexByte(key[start+1:], '}'); end > 0 {
			key = key[start+1 : start+1+end]
		}
	}
	return int(crc16(key)) % SlotCount
}

// HashTag wraps id in braces, so that every key built around it lands in
// the slot of id.
func HashTag(id string) string { return "{" + id + "}" }

// crc16 is CRC-16/XMODEM (polynomial 0x1021, initial value 0), the checksum
// of Redis Cluster.
func crc16(s string) uint16 {
	var crc uint16
	for i := 0; i < len(s); i++ {
		crc ^= uint16(s[i]) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
