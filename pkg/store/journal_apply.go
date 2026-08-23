package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/generation"
	bolt "go.etcd.io/bbolt"
)

var activeProviderRecordKey = []byte("active_provider")

type desiredBatchWire struct {
	Cursor          providerCursorWire    `json:"cursor"`
	ReplaceManaged  bool                  `json:"replace_managed"`
	Mutations       []desiredMutationWire `json:"mutations"`
	RequiredDomains []generation.Domain   `json:"required_domains"`
}

type desiredMutationWire struct {
	Type  generation.MutationType `json:"type"`
	Key   generation.ResourceKey  `json:"key"`
	Value []byte                  `json:"value"`
}

type cursorRecord struct {
	Cursor      generation.ProviderCursor
	BatchDigest [32]byte
	Ticket      generation.ApplyTicket
}

type providerCursorWire struct {
	Provider string `json:"provider"`
	Revision string `json:"revision"`
}

type applyTicketWire struct {
	DesiredRevision uint64              `json:"desired_revision"`
	DesiredDigest   [32]byte            `json:"desired_digest"`
	Cursor          providerCursorWire  `json:"cursor"`
	RequiredDomains []generation.Domain `json:"required_domains"`
}

type cursorRecordWire struct {
	Cursor      providerCursorWire `json:"cursor"`
	BatchDigest [32]byte           `json:"batch_digest"`
	Ticket      applyTicketWire    `json:"ticket"`
}

type cursorRecordEnvelope struct {
	Digest  [32]byte `json:"digest"`
	Payload []byte   `json:"payload"`
}

func (s *Store) ApplyDesired(
	ctx context.Context,
	batch generation.DesiredBatch,
) (generation.ApplyTicket, error) {
	batch = cloneDesiredBatch(batch)
	if err := validateDesiredBatch(batch); err != nil {
		return generation.ApplyTicket{}, err
	}
	if err := contextErr(ctx); err != nil {
		return generation.ApplyTicket{}, err
	}

	var ticket generation.ApplyTicket
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		batchDigest, err := digestDesiredBatch(batch)
		if err != nil {
			return err
		}
		current, err := loadDesiredSnapshotTx(tx, 0)
		if err != nil && !errors.Is(err, generation.ErrNotFound) {
			return err
		}
		activeProvider, err := loadActiveProviderTx(tx, current)
		if err != nil {
			return err
		}
		replay, recordedDigest, found, err := loadCursorTx(tx, batch.Cursor)
		if err != nil {
			return err
		}
		if found {
			if recordedDigest != batchDigest {
				return generation.ErrCursorConflict
			}
			if activeProvider != batch.Cursor.Provider {
				return generation.ErrProviderConflict
			}
			if replay.DesiredRevision != current.Revision() {
				return generation.ErrStaleCursor
			}
			if replay.DesiredDigest != current.Digest() {
				return generation.ErrIntegrity
			}
			ticket = cloneApplyTicket(replay)
			return nil
		}
		if activeProvider != "" && activeProvider != batch.Cursor.Provider && !batch.ReplaceManaged {
			return generation.ErrProviderConflict
		}
		if current.Revision() == math.MaxUint64 {
			return fmt.Errorf("desired revision overflow")
		}
		next, err := applyBatch(current, batch)
		if err != nil {
			return err
		}
		ticket = generation.ApplyTicket{
			DesiredRevision: next.Revision(),
			DesiredDigest:   next.Digest(),
			Cursor:          batch.Cursor,
			RequiredDomains: normalizeDomains(batch.RequiredDomains),
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		return persistDesiredTx(tx, next, ticket, batchDigest)
	})
	if err != nil {
		return generation.ApplyTicket{}, err
	}
	return cloneApplyTicket(ticket), nil
}

func (s *Store) LoadDesired(ctx context.Context, revision uint64) (generation.Snapshot, error) {
	if err := contextErr(ctx); err != nil {
		return generation.Snapshot{}, err
	}
	var snapshot generation.Snapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		loaded, err := loadDesiredSnapshotTx(tx, revision)
		if err != nil {
			return err
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		snapshot = loaded
		return nil
	})
	return snapshot, err
}

func validateDesiredBatch(batch generation.DesiredBatch) error {
	if err := validateProviderCursor(batch.Cursor); err != nil {
		return err
	}
	for _, mutation := range batch.Mutations {
		if mutation.Key.Kind == "" || mutation.Key.ID == "" ||
			!utf8.ValidString(mutation.Key.Kind) || !utf8.ValidString(mutation.Key.ID) {
			return fmt.Errorf("desired mutation requires a non-empty valid UTF-8 resource key")
		}
		switch mutation.Type {
		case generation.MutationPut:
		case generation.MutationDelete:
			if len(mutation.Value) != 0 {
				return fmt.Errorf("desired delete must not carry a value")
			}
		default:
			return fmt.Errorf("unknown desired mutation type %q", mutation.Type)
		}
	}
	for _, domain := range batch.RequiredDomains {
		if domain == "" || !utf8.ValidString(string(domain)) {
			return fmt.Errorf("desired domain must be non-empty valid UTF-8")
		}
		if domain != generation.DomainHTTP && domain != generation.DomainStream {
			return fmt.Errorf("unknown desired domain %q", domain)
		}
	}
	domains := normalizeDomains(batch.RequiredDomains)
	if batch.ReplaceManaged {
		if !slices.Equal(domains, []generation.Domain{generation.DomainHTTP, generation.DomainStream}) {
			return fmt.Errorf("authoritative replacement requires http and stream domains")
		}
	} else if len(batch.Mutations) != 0 && len(domains) == 0 {
		return fmt.Errorf("incremental desired mutations require a domain")
	}
	return nil
}

func digestDesiredBatch(batch generation.DesiredBatch) ([32]byte, error) {
	if err := validateDesiredBatch(batch); err != nil {
		return [32]byte{}, err
	}
	mutations := make([]desiredMutationWire, 0, len(batch.Mutations))
	for _, mutation := range batch.Mutations {
		value := cloneBytes(mutation.Value)
		if mutation.Type == generation.MutationDelete {
			value = nil
		}
		mutations = append(mutations, desiredMutationWire{
			Type:  mutation.Type,
			Key:   mutation.Key,
			Value: value,
		})
	}
	encoded, err := json.Marshal(desiredBatchWire{
		Cursor:          providerCursorToWire(batch.Cursor),
		ReplaceManaged:  batch.ReplaceManaged,
		Mutations:       mutations,
		RequiredDomains: normalizeDomains(batch.RequiredDomains),
	})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func loadActiveProviderTx(tx *bolt.Tx, current generation.Snapshot) (string, error) {
	bucket := tx.Bucket(providerCursorBucket)
	if bucket == nil {
		return "", generation.ErrIntegrity
	}
	encoded := bucket.Get(activeProviderRecordKey)
	if encoded == nil {
		if bucket.Stats().KeyN != 0 {
			return "", generation.ErrIntegrity
		}
		return "", nil
	}
	record, err := decodeCursorRecord(encoded, nil)
	if err != nil {
		return "", err
	}
	keyed := bucket.Get(cursorRecordKey(record.Cursor))
	if keyed == nil || !bytes.Equal(keyed, encoded) {
		return "", generation.ErrIntegrity
	}
	if record.Ticket.DesiredRevision != current.Revision() ||
		record.Ticket.DesiredDigest != current.Digest() {
		return "", generation.ErrIntegrity
	}
	return record.Cursor.Provider, nil
}

func loadCursorTx(
	tx *bolt.Tx,
	cursor generation.ProviderCursor,
) (generation.ApplyTicket, [32]byte, bool, error) {
	bucket := tx.Bucket(providerCursorBucket)
	if bucket == nil {
		return generation.ApplyTicket{}, [32]byte{}, false, generation.ErrIntegrity
	}
	encoded := bucket.Get(cursorRecordKey(cursor))
	if encoded == nil {
		return generation.ApplyTicket{}, [32]byte{}, false, nil
	}
	record, err := decodeCursorRecord(encoded, &cursor)
	if err != nil {
		return generation.ApplyTicket{}, [32]byte{}, false, err
	}
	return cloneApplyTicket(record.Ticket), record.BatchDigest, true, nil
}

func applyBatch(
	current generation.Snapshot,
	batch generation.DesiredBatch,
) (generation.Snapshot, error) {
	if current.Revision() == math.MaxUint64 {
		return generation.Snapshot{}, fmt.Errorf("desired revision overflow")
	}
	nextRevision := current.Revision() + 1
	resources := make(map[generation.ResourceKey][]byte)
	for _, resource := range current.Resources() {
		resources[resource.Key] = cloneBytes(resource.Value)
	}
	tombstones := make(map[generation.ResourceKey]uint64)
	for _, tombstone := range current.Tombstones() {
		tombstones[tombstone.Key] = tombstone.Revision
	}
	if batch.ReplaceManaged {
		for key := range resources {
			delete(resources, key)
			tombstones[key] = nextRevision
		}
	}
	for _, mutation := range batch.Mutations {
		switch mutation.Type {
		case generation.MutationPut:
			resources[mutation.Key] = cloneBytes(mutation.Value)
			delete(tombstones, mutation.Key)
		case generation.MutationDelete:
			delete(resources, mutation.Key)
			tombstones[mutation.Key] = nextRevision
		default:
			return generation.Snapshot{}, fmt.Errorf("unknown desired mutation type %q", mutation.Type)
		}
	}

	resourceList := make([]generation.Resource, 0, len(resources))
	for key, value := range resources {
		resourceList = append(resourceList, generation.Resource{Key: key, Value: cloneBytes(value)})
	}
	tombstoneList := make([]generation.Tombstone, 0, len(tombstones))
	for key, revision := range tombstones {
		tombstoneList = append(tombstoneList, generation.Tombstone{Key: key, Revision: revision})
	}
	return generation.NewSnapshot(nextRevision, resourceList, tombstoneList)
}

func normalizeDomains(domains []generation.Domain) []generation.Domain {
	httpRequired := false
	streamRequired := false
	for _, domain := range domains {
		switch domain {
		case generation.DomainHTTP:
			httpRequired = true
		case generation.DomainStream:
			streamRequired = true
		}
	}
	result := make([]generation.Domain, 0, 2)
	if httpRequired {
		result = append(result, generation.DomainHTTP)
	}
	if streamRequired {
		result = append(result, generation.DomainStream)
	}
	return result
}

func persistDesiredTx(
	tx *bolt.Tx,
	snapshot generation.Snapshot,
	ticket generation.ApplyTicket,
	batchDigest [32]byte,
) error {
	if snapshot.Revision() == 0 || ticket.DesiredRevision != snapshot.Revision() ||
		ticket.DesiredDigest != snapshot.Digest() {
		return generation.ErrIntegrity
	}
	if err := validateProviderCursor(ticket.Cursor); err != nil {
		return generation.ErrIntegrity
	}
	normalizedDomains := normalizeDomains(ticket.RequiredDomains)
	if !slices.Equal(ticket.RequiredDomains, normalizedDomains) {
		return generation.ErrIntegrity
	}
	record := cursorRecord{
		Cursor:      ticket.Cursor,
		BatchDigest: batchDigest,
		Ticket:      cloneApplyTicket(ticket),
	}
	encoded, err := encodeCursorRecord(record)
	if err != nil {
		return err
	}
	bucket := tx.Bucket(providerCursorBucket)
	if bucket == nil {
		return generation.ErrIntegrity
	}
	key := cursorRecordKey(ticket.Cursor)
	if bucket.Get(key) != nil {
		return generation.ErrIntegrity
	}
	if err := writeDesiredHeadTx(tx, snapshot); err != nil {
		return err
	}
	if err := bucket.Put(key, encoded); err != nil {
		return err
	}
	return bucket.Put(activeProviderRecordKey, encoded)
}

func contextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func cursorRecordKey(cursor generation.ProviderCursor) []byte {
	encoded, _ := json.Marshal(struct {
		Provider string `json:"provider"`
		Revision string `json:"revision"`
	}{Provider: cursor.Provider, Revision: cursor.Revision})
	digest := sha256.Sum256(encoded)
	return []byte("cursor:" + hex.EncodeToString(digest[:]))
}

func encodeCursorRecord(record cursorRecord) ([]byte, error) {
	payload, err := json.Marshal(cursorRecordToWire(record))
	if err != nil {
		return nil, err
	}
	envelope := cursorRecordEnvelope{Digest: sha256.Sum256(payload), Payload: payload}
	return json.Marshal(envelope)
}

func decodeCursorRecord(encoded []byte, expected *generation.ProviderCursor) (cursorRecord, error) {
	var envelope cursorRecordEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return cursorRecord{}, generation.ErrIntegrity
	}
	if sha256.Sum256(envelope.Payload) != envelope.Digest {
		return cursorRecord{}, generation.ErrIntegrity
	}
	var wire cursorRecordWire
	if err := json.Unmarshal(envelope.Payload, &wire); err != nil {
		return cursorRecord{}, generation.ErrIntegrity
	}
	canonical, err := json.Marshal(wire)
	if err != nil || !bytes.Equal(canonical, envelope.Payload) {
		return cursorRecord{}, generation.ErrIntegrity
	}
	record := cursorRecordFromWire(wire)
	if err := validateProviderCursor(record.Cursor); err != nil ||
		record.Ticket.DesiredRevision == 0 || record.Ticket.Cursor != record.Cursor {
		return cursorRecord{}, generation.ErrIntegrity
	}
	for _, domain := range record.Ticket.RequiredDomains {
		if domain != generation.DomainHTTP && domain != generation.DomainStream {
			return cursorRecord{}, generation.ErrIntegrity
		}
	}
	if !slices.Equal(record.Ticket.RequiredDomains, normalizeDomains(record.Ticket.RequiredDomains)) {
		return cursorRecord{}, generation.ErrIntegrity
	}
	if expected != nil && record.Cursor != *expected {
		return cursorRecord{}, generation.ErrIntegrity
	}
	return record, nil
}

func validateProviderCursor(cursor generation.ProviderCursor) error {
	if cursor.Provider == "" || cursor.Revision == "" ||
		!utf8.ValidString(cursor.Provider) || !utf8.ValidString(cursor.Revision) {
		return fmt.Errorf("desired cursor requires non-empty valid UTF-8 provider and revision")
	}
	return nil
}

func providerCursorToWire(cursor generation.ProviderCursor) providerCursorWire {
	return providerCursorWire{Provider: cursor.Provider, Revision: cursor.Revision}
}

func providerCursorFromWire(wire providerCursorWire) generation.ProviderCursor {
	return generation.ProviderCursor{Provider: wire.Provider, Revision: wire.Revision}
}

func applyTicketToWire(ticket generation.ApplyTicket) applyTicketWire {
	return applyTicketWire{
		DesiredRevision: ticket.DesiredRevision,
		DesiredDigest:   ticket.DesiredDigest,
		Cursor:          providerCursorToWire(ticket.Cursor),
		RequiredDomains: slices.Clone(ticket.RequiredDomains),
	}
}

func applyTicketFromWire(wire applyTicketWire) generation.ApplyTicket {
	return generation.ApplyTicket{
		DesiredRevision: wire.DesiredRevision,
		DesiredDigest:   wire.DesiredDigest,
		Cursor:          providerCursorFromWire(wire.Cursor),
		RequiredDomains: slices.Clone(wire.RequiredDomains),
	}
}

func cursorRecordToWire(record cursorRecord) cursorRecordWire {
	return cursorRecordWire{
		Cursor:      providerCursorToWire(record.Cursor),
		BatchDigest: record.BatchDigest,
		Ticket:      applyTicketToWire(record.Ticket),
	}
}

func cursorRecordFromWire(wire cursorRecordWire) cursorRecord {
	return cursorRecord{
		Cursor:      providerCursorFromWire(wire.Cursor),
		BatchDigest: wire.BatchDigest,
		Ticket:      applyTicketFromWire(wire.Ticket),
	}
}

func cloneDesiredBatch(batch generation.DesiredBatch) generation.DesiredBatch {
	clone := batch
	clone.Mutations = slices.Clone(batch.Mutations)
	for index := range clone.Mutations {
		clone.Mutations[index].Value = cloneBytes(clone.Mutations[index].Value)
	}
	clone.RequiredDomains = slices.Clone(batch.RequiredDomains)
	return clone
}

func cloneApplyTicket(ticket generation.ApplyTicket) generation.ApplyTicket {
	clone := ticket
	clone.RequiredDomains = slices.Clone(ticket.RequiredDomains)
	return clone
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	clone := make([]byte, len(value))
	copy(clone, value)
	return clone
}
