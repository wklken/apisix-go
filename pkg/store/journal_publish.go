package store

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	bolt "go.etcd.io/bbolt"
)

const publicationTokenAttempts = 16

var publicationTokenReader io.Reader = cryptorand.Reader

type stagedPublication struct {
	Token  generation.PublicationToken
	Ticket generation.ApplyTicket
	Set    generation.PublicationSet
}

type resourceKeyWire struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type generationArtifactWire struct {
	Domain   generation.Domain `json:"domain"`
	Revision uint64            `json:"revision"`
	Digest   [32]byte          `json:"digest"`
	Snapshot string            `json:"snapshot"`
}

type resourceDecisionWire struct {
	Key         resourceKeyWire                `json:"key"`
	Disposition generation.ResourceDisposition `json:"disposition"`
	Code        string                         `json:"code"`
}

type publicationCandidateWire struct {
	Artifact        generationArtifactWire `json:"artifact"`
	SnapshotPayload []byte                 `json:"snapshot_payload"`
	Closure         []resourceKeyWire      `json:"closure"`
	Decisions       []resourceDecisionWire `json:"decisions"`
}

type publicationDomainWire struct {
	Domain    generation.Domain        `json:"domain"`
	Candidate publicationCandidateWire `json:"candidate"`
}

type stagedPublicationWire struct {
	Token           generation.PublicationToken `json:"token"`
	Ticket          applyTicketWire             `json:"ticket"`
	DesiredRevision uint64                      `json:"desired_revision"`
	Domains         []publicationDomainWire     `json:"domains"`
}

type publicationEnvelope struct {
	Digest  [32]byte `json:"digest"`
	Size    uint64   `json:"size"`
	Payload []byte   `json:"payload"`
}

type publishedHeadWire struct {
	Artifact        generationArtifactWire `json:"artifact"`
	Closure         []resourceKeyWire      `json:"closure"`
	DecisionsDigest [32]byte               `json:"decisions_digest"`
}

type publishedDecisionsWire struct {
	Domain    generation.Domain      `json:"domain"`
	Revision  uint64                 `json:"revision"`
	Decisions []resourceDecisionWire `json:"decisions"`
}

func (s *Store) Stage(
	ctx context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (generation.PublicationToken, error) {
	ticket = cloneApplyTicket(ticket)
	set = clonePublicationSet(set)
	if err := contextErr(ctx); err != nil {
		return "", err
	}
	var token generation.PublicationToken
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := validateStageTicketTx(tx, ticket, false); err != nil {
			return err
		}
		record, err := loadCursorRecordForTicketTx(tx, ticket)
		if err != nil {
			return err
		}
		if record.Committed != nil {
			return generation.ErrStaleCursor
		}
		if err := generation.ValidatePublicationSet(ticket, set); err != nil {
			return err
		}
		if err := validateNewDecisionCodes(set); err != nil {
			return err
		}
		desired, err := loadDesiredSnapshotTx(tx, ticket.DesiredRevision)
		if err != nil {
			return err
		}
		if err := validatePublicationPolicyTx(tx, desired, set); err != nil {
			return err
		}
		bucket := tx.Bucket(publicationTxnBucket)
		if bucket == nil {
			return generation.ErrIntegrity
		}
		for range publicationTokenAttempts {
			generated, err := generatePublicationToken()
			if err != nil {
				return err
			}
			if bucket.Get([]byte(generated)) != nil {
				continue
			}
			encoded, err := encodeStagedPublication(stagedPublication{Token: generated, Ticket: ticket, Set: set})
			if err != nil {
				return err
			}
			if err := contextErr(ctx); err != nil {
				return err
			}
			if err := bucket.Put([]byte(generated), encoded); err != nil {
				return err
			}
			if err := contextErr(ctx); err != nil {
				return err
			}
			token = generated
			return nil
		}
		return fmt.Errorf("publication token collision limit exceeded")
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) LoadAcknowledgement(
	ctx context.Context,
	cursor generation.ProviderCursor,
) (generation.Acknowledgement, error) {
	if err := contextErr(ctx); err != nil {
		return generation.Acknowledgement{}, err
	}
	if err := validateProviderCursor(cursor); err != nil {
		return generation.Acknowledgement{}, generation.ErrNotFound
	}
	var acknowledgement generation.Acknowledgement
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		bucket := tx.Bucket(providerCursorBucket)
		if bucket == nil {
			return generation.ErrIntegrity
		}
		keyed := bucket.Get(cursorRecordKey(cursor))
		if keyed == nil {
			return generation.ErrNotFound
		}
		record, err := decodeCursorRecord(keyed, &cursor)
		if err != nil {
			return err
		}
		activeEncoded := bucket.Get(activeProviderRecordKey)
		if activeEncoded == nil {
			return generation.ErrIntegrity
		}
		active, err := decodeCursorRecord(activeEncoded, nil)
		if err != nil {
			return err
		}
		activeKeyed := bucket.Get(cursorRecordKey(active.Cursor))
		if activeKeyed == nil || !bytes.Equal(activeKeyed, activeEncoded) {
			return generation.ErrIntegrity
		}
		desired, err := loadDesiredSnapshotTx(tx, 0)
		if err != nil || active.Ticket.DesiredRevision != desired.Revision() ||
			active.Ticket.DesiredDigest != desired.Digest() {
			return generation.ErrIntegrity
		}
		if active.Cursor != cursor {
			return generation.ErrStaleCursor
		}
		if !bytes.Equal(keyed, activeEncoded) {
			return generation.ErrIntegrity
		}
		if record.Committed == nil {
			backfilled, found, err := backfillMarkerlessAcknowledgementTx(tx, record)
			if err != nil {
				return err
			}
			if !found {
				return generation.ErrNotFound
			}
			record.Committed = &backfilled
		}
		if err := validateCommittedAcknowledgement(record); err != nil {
			return err
		}
		committed := cloneAcknowledgement(*record.Committed)
		revisions, err := loadRevisionSetTx(tx)
		if err != nil {
			return err
		}
		if committed.Revisions != revisions || committed.Cursor != cursor ||
			committed.Revisions.Desired != record.Ticket.DesiredRevision ||
			len(committed.Decisions) != len(record.Ticket.RequiredDomains) {
			return generation.ErrIntegrity
		}
		for _, domain := range record.Ticket.RequiredDomains {
			published, err := loadPublishedTx(tx, domain)
			if err != nil {
				return generation.ErrIntegrity
			}
			decisions, found := committed.Decisions[domain]
			if !found ||
				published.Artifact.Revision != revisionForDomain(committed.Revisions, domain) ||
				published.Artifact.Revision != record.Ticket.DesiredRevision ||
				!slices.Equal(decisions, canonicalDecisions(published.Decisions)) {
				return generation.ErrIntegrity
			}
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		acknowledgement = committed
		return nil
	})
	if err != nil {
		return generation.Acknowledgement{}, err
	}
	return cloneAcknowledgement(acknowledgement), nil
}

func backfillMarkerlessAcknowledgementTx(
	tx *bolt.Tx,
	record cursorRecord,
) (generation.Acknowledgement, bool, error) {
	if len(record.Ticket.RequiredDomains) == 0 {
		return generation.Acknowledgement{}, false, nil
	}
	revisions, err := loadRevisionSetTx(tx)
	if err != nil {
		return generation.Acknowledgement{}, false, err
	}
	if revisions.Desired != record.Ticket.DesiredRevision {
		return generation.Acknowledgement{}, false, generation.ErrIntegrity
	}
	ack := generation.Acknowledgement{
		Cursor: record.Cursor, Revisions: revisions,
		Decisions: make(map[generation.Domain][]generation.ResourceDecision, len(record.Ticket.RequiredDomains)),
	}
	exact := 0
	for _, domain := range record.Ticket.RequiredDomains {
		published, loadErr := loadPublishedTx(tx, domain)
		if errors.Is(loadErr, generation.ErrNotFound) {
			continue
		}
		if loadErr != nil {
			return generation.Acknowledgement{}, false, loadErr
		}
		if published.Artifact.Revision < record.Ticket.DesiredRevision {
			continue
		}
		if published.Artifact.Revision > record.Ticket.DesiredRevision ||
			revisionForDomain(revisions, domain) != record.Ticket.DesiredRevision {
			return generation.Acknowledgement{}, false, generation.ErrIntegrity
		}
		exact++
		ack.Decisions[domain] = canonicalDecisions(published.Decisions)
	}
	if exact == 0 {
		return generation.Acknowledgement{}, false, nil
	}
	if exact != len(record.Ticket.RequiredDomains) {
		return generation.Acknowledgement{}, false, generation.ErrIntegrity
	}
	if err := putCommittedAcknowledgementTx(tx, record.Ticket, ack); err != nil {
		return generation.Acknowledgement{}, false, err
	}
	return cloneAcknowledgement(ack), true, nil
}

func (s *Store) Commit(
	ctx context.Context,
	token generation.PublicationToken,
) (generation.Acknowledgement, error) {
	if err := validatePublicationToken(token); err != nil {
		return generation.Acknowledgement{}, err
	}
	if err := contextErr(ctx); err != nil {
		return generation.Acknowledgement{}, err
	}
	var ack generation.Acknowledgement
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		staged, err := loadStagedPublicationTx(tx, token)
		if err != nil {
			return err
		}
		if err := validateStageTicketTx(tx, staged.Ticket, true); err != nil {
			return err
		}
		record, err := loadCursorRecordForTicketTx(tx, staged.Ticket)
		if err != nil {
			return err
		}
		if record.Committed != nil {
			return generation.ErrStaleCursor
		}
		desired, err := loadDesiredSnapshotTx(tx, staged.Ticket.DesiredRevision)
		if err != nil {
			return err
		}
		if err := validatePublicationPolicyTx(tx, desired, staged.Set); err != nil {
			return err
		}
		current, err := loadRevisionSetTx(tx)
		if err != nil {
			return err
		}
		for _, domain := range sortedPublicationDomains(staged.Set.Domains) {
			candidate := staged.Set.Domains[domain]
			if candidate.Artifact.Revision <= revisionForDomain(current, domain) {
				return generation.ErrStaleCursor
			}
			if err := putArtifactTx(tx, candidate.Snapshot); err != nil {
				return err
			}
			if err := putPublishedHeadTx(tx, domain, candidate); err != nil {
				return err
			}
			if err := putDecisionsTx(tx, domain, candidate.Artifact.Revision, candidate.Decisions); err != nil {
				return err
			}
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := deleteStagedPublicationTx(tx, token); err != nil {
			return err
		}
		revisions, err := loadRevisionSetTx(tx)
		if err != nil {
			return err
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		ack = acknowledgementFrom(staged)
		ack.Revisions = revisions
		if err := putCommittedAcknowledgementTx(tx, staged.Ticket, ack); err != nil {
			return err
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return generation.Acknowledgement{}, err
	}
	return cloneAcknowledgement(ack), nil
}

func (s *Store) Abort(ctx context.Context, token generation.PublicationToken, reason string) error {
	if err := validatePublicationToken(token); err != nil {
		return err
	}
	if reason == "" || len(reason) > 128 || !utf8.ValidString(reason) {
		return fmt.Errorf("publication abort reason must be non-empty valid UTF-8 within 128 bytes")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := deleteStagedPublicationTx(tx, token); err != nil {
			return err
		}
		return contextErr(ctx)
	})
}

func (s *Store) LoadPublished(
	ctx context.Context,
	domain generation.Domain,
) (generation.PublishedGeneration, error) {
	if !validPublicationDomain(domain) {
		return generation.PublishedGeneration{}, generation.ErrNotFound
	}
	if err := contextErr(ctx); err != nil {
		return generation.PublishedGeneration{}, err
	}
	var published generation.PublishedGeneration
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		loaded, err := loadPublishedTx(tx, domain)
		if err != nil {
			return err
		}
		if err := contextErr(ctx); err != nil {
			return err
		}
		published = loaded
		return nil
	})
	if err != nil {
		return generation.PublishedGeneration{}, err
	}
	return clonePublishedGeneration(published), nil
}

func validatePublicationPolicyTx(
	tx *bolt.Tx,
	desired generation.Snapshot,
	set generation.PublicationSet,
) error {
	for _, domain := range sortedPublicationDomains(set.Domains) {
		predecessor, err := loadPublishedTx(tx, domain)
		if err != nil && !errors.Is(err, generation.ErrNotFound) {
			return err
		}
		hasPredecessor := err == nil
		if err := validatePublicationCandidatePolicy(
			desired,
			set.Domains[domain],
			predecessor,
			hasPredecessor,
		); err != nil {
			return err
		}
	}
	return nil
}

func validatePublicationCandidatePolicy(
	desired generation.Snapshot,
	candidate generation.PublicationCandidate,
	predecessor generation.PublishedGeneration,
	hasPredecessor bool,
) error {
	desiredResources := resourceValues(desired.Resources())
	desiredTombstones := tombstoneRevisions(desired.Tombstones())
	candidateResources := resourceValues(candidate.Snapshot.Resources())
	candidateTombstones := tombstoneRevisions(candidate.Snapshot.Tombstones())
	predecessorResources := resourceValues(predecessor.Snapshot.Resources())

	for _, decision := range candidate.Decisions {
		desiredValue, desiredLive := desiredResources[decision.Key]
		desiredDeletedRevision, desiredDeleted := desiredTombstones[decision.Key]
		candidateValue, candidateLive := candidateResources[decision.Key]
		candidateDeletedRevision, candidateDeleted := candidateTombstones[decision.Key]

		if !desiredLive && !desiredDeleted {
			return generation.ErrInvalidClosure
		}
		switch decision.Disposition {
		case generation.DispositionPublished:
			if !desiredLive || desiredDeleted || !candidateLive || candidateDeleted ||
				!exactBytes(candidateValue, desiredValue) {
				return generation.ErrInvalidClosure
			}
		case generation.DispositionLastGood:
			if !desiredLive || desiredDeleted {
				return generation.ErrNoLastGood
			}
			if !candidateLive || candidateDeleted {
				return generation.ErrInvalidClosure
			}
			predecessorValue, found := predecessorResources[decision.Key]
			if !hasPredecessor || !found || !exactBytes(candidateValue, predecessorValue) {
				return generation.ErrNoLastGood
			}
		case generation.DispositionQuarantined, generation.DispositionFailClosed:
			if !desiredLive || desiredDeleted || candidateLive || candidateDeleted {
				return generation.ErrInvalidClosure
			}
		case generation.DispositionDeleted:
			if desiredLive || !desiredDeleted || candidateLive || !candidateDeleted ||
				candidateDeletedRevision != desiredDeletedRevision {
				return generation.ErrInvalidClosure
			}
		default:
			return generation.ErrInvalidClosure
		}
	}
	return nil
}

func exactBytes(left, right []byte) bool {
	return (left == nil) == (right == nil) && bytes.Equal(left, right)
}

func validateNewDecisionCodes(set generation.PublicationSet) error {
	for _, domain := range sortedPublicationDomains(set.Domains) {
		for _, decision := range set.Domains[domain].Decisions {
			if !validDecisionCode(decision.Code) {
				return generation.ErrInvalidClosure
			}
		}
	}
	return nil
}

func resourceValues(resources []generation.Resource) map[generation.ResourceKey][]byte {
	values := make(map[generation.ResourceKey][]byte, len(resources))
	for _, resource := range resources {
		values[resource.Key] = resource.Value
	}
	return values
}

func tombstoneRevisions(tombstones []generation.Tombstone) map[generation.ResourceKey]uint64 {
	revisions := make(map[generation.ResourceKey]uint64, len(tombstones))
	for _, tombstone := range tombstones {
		revisions[tombstone.Key] = tombstone.Revision
	}
	return revisions
}

func validateStageTicketTx(tx *bolt.Tx, ticket generation.ApplyTicket, staleIsCursor bool) error {
	current, err := loadDesiredSnapshotTx(tx, 0)
	if err != nil {
		if errors.Is(err, generation.ErrNotFound) {
			return generation.ErrIntegrity
		}
		return err
	}
	if ticket.DesiredRevision != current.Revision() {
		if staleIsCursor || ticket.DesiredRevision < current.Revision() {
			return generation.ErrStaleCursor
		}
		return generation.ErrIntegrity
	}
	if ticket.DesiredDigest != current.Digest() {
		return generation.ErrIntegrity
	}
	bucket := tx.Bucket(providerCursorBucket)
	if bucket == nil {
		return generation.ErrIntegrity
	}
	active, err := decodeCursorRecord(bucket.Get(activeProviderRecordKey), nil)
	if err != nil {
		return err
	}
	if active.Ticket.DesiredRevision != ticket.DesiredRevision ||
		active.Ticket.DesiredDigest != ticket.DesiredDigest ||
		active.Ticket.Cursor != ticket.Cursor ||
		!slices.Equal(active.Ticket.RequiredDomains, ticket.RequiredDomains) {
		return generation.ErrIntegrity
	}
	keyed := bucket.Get(cursorRecordKey(ticket.Cursor))
	if keyed == nil || !bytes.Equal(keyed, bucket.Get(activeProviderRecordKey)) {
		return generation.ErrIntegrity
	}
	return nil
}

func loadStagedPublicationTx(
	tx *bolt.Tx,
	token generation.PublicationToken,
) (stagedPublication, error) {
	if err := validatePublicationToken(token); err != nil {
		return stagedPublication{}, err
	}
	bucket := tx.Bucket(publicationTxnBucket)
	if bucket == nil {
		return stagedPublication{}, generation.ErrIntegrity
	}
	encoded := bucket.Get([]byte(token))
	if encoded == nil {
		return stagedPublication{}, generation.ErrNotFound
	}
	return decodeStagedPublication(encoded, token)
}

func putArtifactTx(tx *bolt.Tx, snapshot generation.Snapshot) error {
	payload, err := snapshot.CanonicalBytes()
	if err != nil {
		return err
	}
	envelope := artifactEnvelope{Digest: snapshot.Digest(), Size: uint64(len(payload)), Payload: payload}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	bucket := tx.Bucket(artifactBucket)
	if bucket == nil {
		return generation.ErrIntegrity
	}
	key := []byte(snapshot.SnapshotID())
	if existing := bucket.Get(key); existing != nil && !bytes.Equal(existing, encoded) {
		return generation.ErrIntegrity
	}
	return bucket.Put(key, encoded)
}

func putPublishedHeadTx(
	tx *bolt.Tx,
	domain generation.Domain,
	candidate generation.PublicationCandidate,
) error {
	decisionPayload, err := encodeDecisionsPayload(domain, candidate.Artifact.Revision, candidate.Decisions)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(publishedHeadWire{
		Artifact:        artifactToWire(candidate.Artifact),
		Closure:         resourceKeysToWire(canonicalResourceKeys(candidate.Closure)),
		DecisionsDigest: sha256.Sum256(decisionPayload),
	})
	if err != nil {
		return err
	}
	encoded, err := encodePublicationEnvelope(payload)
	if err != nil {
		return err
	}
	bucket := tx.Bucket(publishedHeadBucket)
	if bucket == nil {
		return generation.ErrIntegrity
	}
	return bucket.Put([]byte(domain), encoded)
}

func putDecisionsTx(
	tx *bolt.Tx,
	domain generation.Domain,
	revision uint64,
	decisions []generation.ResourceDecision,
) error {
	payload, err := encodeDecisionsPayload(domain, revision, decisions)
	if err != nil {
		return err
	}
	encoded, err := encodePublicationEnvelope(payload)
	if err != nil {
		return err
	}
	bucket := tx.Bucket(publicationDecisionBucket)
	if bucket == nil {
		return generation.ErrIntegrity
	}
	key := publicationDecisionKey(domain, revision)
	if existing := bucket.Get(key); existing != nil && !bytes.Equal(existing, encoded) {
		return generation.ErrIntegrity
	}
	return bucket.Put(key, encoded)
}

func deleteStagedPublicationTx(tx *bolt.Tx, token generation.PublicationToken) error {
	bucket := tx.Bucket(publicationTxnBucket)
	if bucket == nil {
		return generation.ErrIntegrity
	}
	if bucket.Get([]byte(token)) == nil {
		return generation.ErrNotFound
	}
	return bucket.Delete([]byte(token))
}

func acknowledgementFrom(staged stagedPublication) generation.Acknowledgement {
	ack := generation.Acknowledgement{
		Cursor:    staged.Ticket.Cursor,
		Decisions: make(map[generation.Domain][]generation.ResourceDecision, len(staged.Set.Domains)),
	}
	for _, domain := range sortedPublicationDomains(staged.Set.Domains) {
		ack.Decisions[domain] = canonicalDecisions(staged.Set.Domains[domain].Decisions)
	}
	return ack
}

func loadRevisionSetTx(tx *bolt.Tx) (generation.RevisionSet, error) {
	var revisions generation.RevisionSet
	published := tx.Bucket(publishedHeadBucket)
	if published == nil {
		return revisions, generation.ErrIntegrity
	}
	desired, err := loadDesiredSnapshotTx(tx, 0)
	if err != nil && !errors.Is(err, generation.ErrNotFound) {
		return revisions, err
	}
	if err == nil {
		revisions.Desired = desired.Revision()
	}
	if err := published.ForEach(func(key, value []byte) error {
		domain := generation.Domain(key)
		if !validPublicationDomain(domain) || value == nil {
			return generation.ErrIntegrity
		}
		head, err := decodePublishedHead(value, domain)
		if err != nil {
			return err
		}
		switch domain {
		case generation.DomainHTTP:
			revisions.HTTP = head.Artifact.Revision
		case generation.DomainStream:
			revisions.Stream = head.Artifact.Revision
		}
		return nil
	}); err != nil {
		return generation.RevisionSet{}, err
	}
	if revisions.HTTP > revisions.Desired || revisions.Stream > revisions.Desired {
		return generation.RevisionSet{}, generation.ErrIntegrity
	}
	return revisions, nil
}

func loadPublishedTx(tx *bolt.Tx, domain generation.Domain) (generation.PublishedGeneration, error) {
	bucket := tx.Bucket(publishedHeadBucket)
	if bucket == nil {
		return generation.PublishedGeneration{}, generation.ErrIntegrity
	}
	encoded := bucket.Get([]byte(domain))
	if encoded == nil {
		return generation.PublishedGeneration{}, generation.ErrNotFound
	}
	head, err := decodePublishedHead(encoded, domain)
	if err != nil {
		return generation.PublishedGeneration{}, err
	}
	snapshot, err := loadArtifactByIDTx(tx, head.Artifact.Snapshot)
	if err != nil {
		return generation.PublishedGeneration{}, err
	}
	decisions, err := loadDecisionsTx(tx, domain, head.Artifact.Revision, head.DecisionsDigest)
	if err != nil {
		return generation.PublishedGeneration{}, err
	}
	published := generation.PublishedGeneration{
		Artifact: head.Artifact, Snapshot: snapshot, Closure: head.Closure, Decisions: decisions,
	}
	if err := generation.ValidatePublicationCandidate(
		domain,
		head.Artifact.Revision,
		generation.PublicationCandidate(published),
	); err != nil {
		return generation.PublishedGeneration{}, generation.ErrIntegrity
	}
	return published, nil
}

type decodedPublishedHead struct {
	Artifact        generation.GenerationArtifact
	Closure         []generation.ResourceKey
	DecisionsDigest [32]byte
}

func decodePublishedHead(encoded []byte, domain generation.Domain) (decodedPublishedHead, error) {
	payload, err := decodePublicationEnvelope(encoded)
	if err != nil {
		return decodedPublishedHead{}, err
	}
	var wire publishedHeadWire
	if err := json.Unmarshal(payload, &wire); err != nil {
		return decodedPublishedHead{}, generation.ErrIntegrity
	}
	canonical, err := json.Marshal(wire)
	if err != nil || !bytes.Equal(canonical, payload) {
		return decodedPublishedHead{}, generation.ErrIntegrity
	}
	result := decodedPublishedHead{
		Artifact: artifactFromWire(wire.Artifact), Closure: resourceKeysFromWire(wire.Closure),
		DecisionsDigest: wire.DecisionsDigest,
	}
	if result.Artifact.Domain != domain || result.Artifact.Revision == 0 ||
		result.Artifact.Digest == ([32]byte{}) || result.DecisionsDigest == ([32]byte{}) ||
		result.Artifact.Snapshot != "sha256:"+hex.EncodeToString(result.Artifact.Digest[:]) ||
		!slices.Equal(result.Closure, canonicalResourceKeys(result.Closure)) {
		return decodedPublishedHead{}, generation.ErrIntegrity
	}
	seen := make(map[generation.ResourceKey]struct{}, len(result.Closure))
	for _, key := range result.Closure {
		if !validResourceKey(key) {
			return decodedPublishedHead{}, generation.ErrIntegrity
		}
		if _, exists := seen[key]; exists {
			return decodedPublishedHead{}, generation.ErrIntegrity
		}
		seen[key] = struct{}{}
	}
	return result, nil
}

func loadDecisionsTx(
	tx *bolt.Tx,
	domain generation.Domain,
	revision uint64,
	expectedDigest [32]byte,
) ([]generation.ResourceDecision, error) {
	bucket := tx.Bucket(publicationDecisionBucket)
	if bucket == nil {
		return nil, generation.ErrIntegrity
	}
	encoded := bucket.Get(publicationDecisionKey(domain, revision))
	if encoded == nil {
		return nil, generation.ErrIntegrity
	}
	payload, err := decodePublicationEnvelope(encoded)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(payload) != expectedDigest {
		return nil, generation.ErrIntegrity
	}
	var wire publishedDecisionsWire
	if err := json.Unmarshal(payload, &wire); err != nil {
		return nil, generation.ErrIntegrity
	}
	canonical, err := json.Marshal(wire)
	if err != nil || !bytes.Equal(canonical, payload) || wire.Domain != domain || wire.Revision != revision {
		return nil, generation.ErrIntegrity
	}
	decisions := decisionsFromWire(wire.Decisions)
	if !slices.EqualFunc(decisions, canonicalDecisions(decisions), func(left, right generation.ResourceDecision) bool {
		return left == right
	}) {
		return nil, generation.ErrIntegrity
	}
	return decisions, nil
}

func encodeDecisionsPayload(
	domain generation.Domain,
	revision uint64,
	decisions []generation.ResourceDecision,
) ([]byte, error) {
	return json.Marshal(publishedDecisionsWire{
		Domain: domain, Revision: revision, Decisions: decisionsToWire(canonicalDecisions(decisions)),
	})
}

func loadArtifactByIDTx(tx *bolt.Tx, id string) (generation.Snapshot, error) {
	bucket := tx.Bucket(artifactBucket)
	if bucket == nil || id == "" {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	encoded := bucket.Get([]byte(id))
	if encoded == nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	var envelope artifactEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil || verifyArtifact(id, envelope) != nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	return snapshotFromPayload(envelope.Payload, id, envelope.Digest)
}

func encodeStagedPublication(staged stagedPublication) ([]byte, error) {
	canonical := cloneStagedPublication(staged)
	if err := generation.ValidatePublicationSet(canonical.Ticket, canonical.Set); err != nil {
		return nil, err
	}
	domains := make([]publicationDomainWire, 0, len(canonical.Set.Domains))
	for _, domain := range sortedPublicationDomains(canonical.Set.Domains) {
		candidate, err := candidateToWire(canonical.Set.Domains[domain])
		if err != nil {
			return nil, err
		}
		domains = append(domains, publicationDomainWire{Domain: domain, Candidate: candidate})
	}
	payload, err := json.Marshal(stagedPublicationWire{
		Token: canonical.Token, Ticket: applyTicketToWire(canonical.Ticket),
		DesiredRevision: canonical.Set.DesiredRevision, Domains: domains,
	})
	if err != nil {
		return nil, err
	}
	return encodePublicationEnvelope(payload)
}

func decodeStagedPublication(
	encoded []byte,
	expectedToken generation.PublicationToken,
) (stagedPublication, error) {
	payload, err := decodePublicationEnvelope(encoded)
	if err != nil {
		return stagedPublication{}, err
	}
	var wire stagedPublicationWire
	if err := json.Unmarshal(payload, &wire); err != nil {
		return stagedPublication{}, generation.ErrIntegrity
	}
	canonical, err := json.Marshal(wire)
	if err != nil || !bytes.Equal(canonical, payload) {
		return stagedPublication{}, generation.ErrIntegrity
	}
	staged := stagedPublication{
		Token:  wire.Token,
		Ticket: applyTicketFromWire(wire.Ticket),
		Set: generation.PublicationSet{
			DesiredRevision: wire.DesiredRevision,
			Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(wire.Domains)),
		},
	}
	if validatePublicationToken(staged.Token) != nil || staged.Token != expectedToken {
		return stagedPublication{}, generation.ErrIntegrity
	}
	lastDomain := generation.Domain("")
	for _, entry := range wire.Domains {
		if !validPublicationDomain(entry.Domain) || entry.Domain <= lastDomain {
			return stagedPublication{}, generation.ErrIntegrity
		}
		candidate, err := candidateFromWire(entry.Candidate)
		if err != nil {
			return stagedPublication{}, err
		}
		staged.Set.Domains[entry.Domain] = candidate
		lastDomain = entry.Domain
	}
	if err := generation.ValidatePublicationSet(staged.Ticket, staged.Set); err != nil {
		return stagedPublication{}, generation.ErrIntegrity
	}
	return staged, nil
}

func candidateToWire(candidate generation.PublicationCandidate) (publicationCandidateWire, error) {
	payload, err := candidate.Snapshot.CanonicalBytes()
	if err != nil {
		return publicationCandidateWire{}, err
	}
	return publicationCandidateWire{
		Artifact: artifactToWire(candidate.Artifact), SnapshotPayload: payload,
		Closure:   resourceKeysToWire(canonicalResourceKeys(candidate.Closure)),
		Decisions: decisionsToWire(canonicalDecisions(candidate.Decisions)),
	}, nil
}

func candidateFromWire(wire publicationCandidateWire) (generation.PublicationCandidate, error) {
	artifact := artifactFromWire(wire.Artifact)
	snapshot, err := snapshotFromPayload(wire.SnapshotPayload, artifact.Snapshot, artifact.Digest)
	if err != nil {
		return generation.PublicationCandidate{}, err
	}
	closure := resourceKeysFromWire(wire.Closure)
	decisions := decisionsFromWire(wire.Decisions)
	if !slices.Equal(closure, canonicalResourceKeys(closure)) ||
		!slices.EqualFunc(
			decisions,
			canonicalDecisions(decisions),
			func(left, right generation.ResourceDecision) bool { return left == right },
		) {
		return generation.PublicationCandidate{}, generation.ErrIntegrity
	}
	return generation.PublicationCandidate{
		Artifact:  artifact,
		Snapshot:  snapshot,
		Closure:   closure,
		Decisions: decisions,
	}, nil
}

func snapshotFromPayload(payload []byte, id string, digest [32]byte) (generation.Snapshot, error) {
	var wire snapshotWire
	if err := json.Unmarshal(payload, &wire); err != nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	snapshot, err := generation.NewSnapshot(wire.Revision, wire.Resources, wire.Tombstones)
	if err != nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	canonical, err := snapshot.CanonicalBytes()
	if err != nil || !bytes.Equal(canonical, payload) || snapshot.Digest() != digest || snapshot.SnapshotID() != id {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	return snapshot, nil
}

func encodePublicationEnvelope(payload []byte) ([]byte, error) {
	return json.Marshal(
		publicationEnvelope{Digest: sha256.Sum256(payload), Size: uint64(len(payload)), Payload: payload},
	)
}

func decodePublicationEnvelope(encoded []byte) ([]byte, error) {
	var envelope publicationEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil || uint64(len(envelope.Payload)) != envelope.Size ||
		sha256.Sum256(envelope.Payload) != envelope.Digest {
		return nil, generation.ErrIntegrity
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return nil, generation.ErrIntegrity
	}
	return bytes.Clone(envelope.Payload), nil
}

func generatePublicationToken() (generation.PublicationToken, error) {
	var random [16]byte
	if _, err := io.ReadFull(publicationTokenReader, random[:]); err != nil {
		return "", fmt.Errorf("generate publication token: %w", err)
	}
	return generation.PublicationToken(hex.EncodeToString(random[:])), nil
}

func validatePublicationToken(token generation.PublicationToken) error {
	if len(token) != 32 || strings.ToLower(string(token)) != string(token) {
		return generation.ErrNotFound
	}
	decoded, err := hex.DecodeString(string(token))
	if err != nil || len(decoded) != 16 {
		return generation.ErrNotFound
	}
	return nil
}

func publicationDecisionKey(domain generation.Domain, revision uint64) []byte {
	key := make([]byte, len(domain)+8)
	copy(key, domain)
	binary.BigEndian.PutUint64(key[len(domain):], revision)
	return key
}

func revisionForDomain(revisions generation.RevisionSet, domain generation.Domain) uint64 {
	if domain == generation.DomainHTTP {
		return revisions.HTTP
	}
	return revisions.Stream
}

func validPublicationDomain(domain generation.Domain) bool {
	return domain == generation.DomainHTTP || domain == generation.DomainStream
}

func validResourceKey(key generation.ResourceKey) bool {
	return key.Kind != "" && key.ID != "" && utf8.ValidString(key.Kind) && utf8.ValidString(key.ID)
}

func validDecisionCode(code string) bool {
	if len(code) == 0 || len(code) > 128 {
		return false
	}
	first := code[0]
	if !lowerAlphaNumeric(first) {
		return false
	}
	for index := 1; index < len(code); index++ {
		character := code[index]
		if lowerAlphaNumeric(character) || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func lowerAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}

func validDisposition(disposition generation.ResourceDisposition) bool {
	switch disposition {
	case generation.DispositionPublished, generation.DispositionLastGood, generation.DispositionQuarantined,
		generation.DispositionFailClosed, generation.DispositionDeleted:
		return true
	default:
		return false
	}
}

func sortedPublicationDomains(domains map[generation.Domain]generation.PublicationCandidate) []generation.Domain {
	result := make([]generation.Domain, 0, len(domains))
	for domain := range domains {
		result = append(result, domain)
	}
	slices.Sort(result)
	return result
}

func canonicalResourceKeys(keys []generation.ResourceKey) []generation.ResourceKey {
	result := slices.Clone(keys)
	slices.SortFunc(result, compareResourceKey)
	return result
}

func canonicalDecisions(decisions []generation.ResourceDecision) []generation.ResourceDecision {
	result := slices.Clone(decisions)
	slices.SortFunc(result, func(left, right generation.ResourceDecision) int {
		return compareResourceKey(left.Key, right.Key)
	})
	return result
}

func compareResourceKey(left, right generation.ResourceKey) int {
	if byKind := strings.Compare(left.Kind, right.Kind); byKind != 0 {
		return byKind
	}
	return strings.Compare(left.ID, right.ID)
}

func resourceKeysToWire(keys []generation.ResourceKey) []resourceKeyWire {
	result := make([]resourceKeyWire, 0, len(keys))
	for _, key := range keys {
		result = append(result, resourceKeyWire{Kind: key.Kind, ID: key.ID})
	}
	return result
}

func resourceKeysFromWire(keys []resourceKeyWire) []generation.ResourceKey {
	result := make([]generation.ResourceKey, 0, len(keys))
	for _, key := range keys {
		result = append(result, generation.ResourceKey{Kind: key.Kind, ID: key.ID})
	}
	return result
}

func decisionsToWire(decisions []generation.ResourceDecision) []resourceDecisionWire {
	result := make([]resourceDecisionWire, 0, len(decisions))
	for _, decision := range decisions {
		result = append(result, resourceDecisionWire{
			Key:         resourceKeyWire{Kind: decision.Key.Kind, ID: decision.Key.ID},
			Disposition: decision.Disposition, Code: decision.Code,
		})
	}
	return result
}

func decisionsFromWire(decisions []resourceDecisionWire) []generation.ResourceDecision {
	result := make([]generation.ResourceDecision, 0, len(decisions))
	for _, decision := range decisions {
		result = append(result, generation.ResourceDecision{
			Key:         generation.ResourceKey{Kind: decision.Key.Kind, ID: decision.Key.ID},
			Disposition: decision.Disposition, Code: decision.Code,
		})
	}
	return result
}

func artifactToWire(artifact generation.GenerationArtifact) generationArtifactWire {
	return generationArtifactWire{
		Domain: artifact.Domain, Revision: artifact.Revision, Digest: artifact.Digest, Snapshot: artifact.Snapshot,
	}
}

func artifactFromWire(wire generationArtifactWire) generation.GenerationArtifact {
	return generation.GenerationArtifact{
		Domain: wire.Domain, Revision: wire.Revision, Digest: wire.Digest, Snapshot: wire.Snapshot,
	}
}

func clonePublicationSet(set generation.PublicationSet) generation.PublicationSet {
	clone := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		clone.Domains[domain] = clonePublicationCandidate(candidate)
	}
	return clone
}

func clonePublicationCandidate(candidate generation.PublicationCandidate) generation.PublicationCandidate {
	clone := candidate
	clone.Snapshot = candidate.Snapshot.Clone()
	clone.Closure = slices.Clone(candidate.Closure)
	clone.Decisions = slices.Clone(candidate.Decisions)
	return clone
}

func cloneStagedPublication(staged stagedPublication) stagedPublication {
	return stagedPublication{
		Token:  staged.Token,
		Ticket: cloneApplyTicket(staged.Ticket),
		Set:    clonePublicationSet(staged.Set),
	}
}

func clonePublishedGeneration(published generation.PublishedGeneration) generation.PublishedGeneration {
	clone := published
	clone.Snapshot = published.Snapshot.Clone()
	clone.Closure = slices.Clone(published.Closure)
	clone.Decisions = slices.Clone(published.Decisions)
	return clone
}

func cloneAcknowledgement(ack generation.Acknowledgement) generation.Acknowledgement {
	clone := ack
	clone.Decisions = make(map[generation.Domain][]generation.ResourceDecision, len(ack.Decisions))
	for domain, decisions := range ack.Decisions {
		clone.Decisions[domain] = slices.Clone(decisions)
	}
	return clone
}
