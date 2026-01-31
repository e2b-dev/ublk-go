package ublk

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRingWrapAroundLogic(t *testing.T) {
	entries := uint32(4)
	var head, tail, ringMask, ringEntries, flags, dropped uint32
	var array [4]uint32
	ringMask, ringEntries = 3, entries

	r := &Ring{
		sq: &submissionQueue{
			head: &head, tail: &tail, ringMask: &ringMask,
			ringEntries: &ringEntries, flags: &flags, dropped: &dropped,
			array: &array[0], sqeHead: 0, sqeTail: 0,
		},
		sqes:    make([]UringSQE, entries),
		sqeSize: SizeOfUringSQE,
	}

	// Fill the ring
	for i := range int(entries) {
		_, err := r.GetSQE()
		require.NoError(t, err, "GetSQE #%d", i)
	}

	// Ring full
	_, err := r.GetSQE()
	require.Error(t, err)
	assert.Equal(t, "submission queue full", err.Error())

	// Simulate kernel consuming 2 entries
	head = 2
	_, err = r.GetSQE()
	require.NoError(t, err, "GetSQE after head advance")
	assert.EqualValues(t, 2, r.sq.sqeHead)
}

func TestRingSQE128WrapAroundLogic(t *testing.T) {
	entries := uint32(4)
	var head, tail, ringMask, ringEntries uint32
	var array [4]uint32
	ringMask, ringEntries = 3, entries
	mmapSQEs := make([]byte, int(entries)*128)

	r := &Ring{
		sq: &submissionQueue{
			head: &head, tail: &tail, ringMask: &ringMask,
			ringEntries: &ringEntries, array: &array[0],
		},
		mmapSQEs: mmapSQEs,
		sqeSize:  128,
	}

	for i := range int(entries) {
		_, err := r.GetSQE128()
		require.NoError(t, err, "GetSQE128 #%d", i)
	}

	_, err := r.GetSQE128()
	require.Error(t, err, "ring should be full")

	head = 1
	_, err = r.GetSQE128()
	require.NoError(t, err)
	assert.EqualValues(t, 1, r.sq.sqeHead)
}
