// Package chash implements the consistent-hash ring used by APISIX HTTP
// upstream owners.
package chash

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"
)

const pointsPerWeight = 160

type Node struct {
	ID     string
	Target string
	Weight int
}

type point struct {
	hash   uint32
	target string
}

type Ring struct {
	points    []point
	nodeCount int
}

func New(nodes []Node) (*Ring, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("chash requires at least one node")
	}
	nodes = append([]Node(nil), nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	gcd := 0
	for i, node := range nodes {
		if node.ID == "" {
			return nil, fmt.Errorf("chash node %d has an empty identity", i)
		}
		if node.Weight <= 0 {
			return nil, fmt.Errorf("chash node %q has non-positive weight %d", node.ID, node.Weight)
		}
		if i > 0 && nodes[i-1].ID == node.ID {
			return nil, fmt.Errorf("duplicate chash node identity %q", node.ID)
		}
		gcd = greatestCommonDivisor(gcd, node.Weight)
	}

	pointCount := 0
	for _, node := range nodes {
		pointCount += node.Weight / gcd * pointsPerWeight
	}
	points := make([]point, 0, pointCount)
	for _, node := range nodes {
		target := node.Target
		if target == "" {
			target = node.ID
		}
		identity := strings.ReplaceAll(node.ID, ":", "\x00")
		baseHash := crc32.ChecksumIEEE([]byte(identity))
		var previousHash uint32
		var previousBytes [4]byte
		for range node.Weight / gcd * pointsPerWeight {
			binary.LittleEndian.PutUint32(previousBytes[:], previousHash)
			pointHash := crc32.Update(baseHash, crc32.IEEETable, previousBytes[:])
			points = append(points, point{hash: pointHash, target: target})
			previousHash = pointHash
		}
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].hash < points[j].hash })
	return &Ring{points: points, nodeCount: len(nodes)}, nil
}

func (r *Ring) Lookup(key string) (string, bool) {
	if r == nil || len(r.points) == 0 {
		return "", false
	}
	return r.points[r.startIndex(key)].target, true
}

// Candidates returns each distinct node once in clockwise ring order. The
// first entry is the normal selection and later entries are deterministic
// retry candidates.
func (r *Ring) Candidates(key string) []string {
	if r == nil || len(r.points) == 0 {
		return nil
	}
	candidates := make([]string, 0, r.nodeCount)
	seen := make(map[string]struct{}, r.nodeCount)
	start := r.startIndex(key)
	for offset := range len(r.points) {
		target := r.points[(start+offset)%len(r.points)].target
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		candidates = append(candidates, target)
		if len(candidates) == r.nodeCount {
			break
		}
	}
	return candidates
}

func (r *Ring) startIndex(key string) int {
	hash := crc32.ChecksumIEEE([]byte(key))
	index := sort.Search(len(r.points), func(i int) bool { return r.points[i].hash >= hash })
	if index == len(r.points) {
		return 0
	}
	return index
}

func greatestCommonDivisor(left, right int) int {
	for right != 0 {
		left, right = right, left%right
	}
	return left
}
