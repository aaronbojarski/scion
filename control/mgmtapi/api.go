// Copyright 2020 Anapaya Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mgmtapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/scionproto/scion/control/beacon"
	cstrust "github.com/scionproto/scion/control/trust"
	"github.com/scionproto/scion/pkg/addr"
	"github.com/scionproto/scion/pkg/private/serrors"
	"github.com/scionproto/scion/pkg/scrypto/cppki"
	seg "github.com/scionproto/scion/pkg/segment"
	"github.com/scionproto/scion/private/ca/renewal"
	api "github.com/scionproto/scion/private/mgmtapi"
	cppkiapi "github.com/scionproto/scion/private/mgmtapi/cppki/api"
	healthapi "github.com/scionproto/scion/private/mgmtapi/health/api"
	segapi "github.com/scionproto/scion/private/mgmtapi/segments/api"
	"github.com/scionproto/scion/private/storage"
	beaconstorage "github.com/scionproto/scion/private/storage/beacon"
	"github.com/scionproto/scion/private/trust"
)

type BeaconStore interface {
	GetBeacons(context.Context, *beaconstorage.QueryParams) ([]beaconstorage.Beacon, error)
	DeleteBeacon(ctx context.Context, idPrefix string) error
}

type Healther interface {
	GetSignerHealth(context.Context) SignerHealthData
	GetTRCHealth(context.Context) TRCHealthData
	GetCAHealth(context.Context) (CAHealthStatus, bool)
}

// SignerHealthData is used to extract the relevant signer data for the signer health check.
type SignerHealthData struct {
	SignerMissing       bool
	SignerMissingDetail string
	Expiration          time.Time
	InGrace             bool
}

// TRCHealthData is used to extract the relevant TRC data for the TRC health check.
type TRCHealthData struct {
	TRCNotFound       bool
	TRCNotFoundDetail string
	TRCID             cppki.TRCID
}

type CAHealthStatus string

const (
	Available   CAHealthStatus = "available"
	Starting    CAHealthStatus = "starting"
	Stopping    CAHealthStatus = "stopping"
	Unavailable CAHealthStatus = "unavailable"
)

// Server implements the Control Service API.
type Server struct {
	SegmentsServer segapi.Server
	CPPKIServer    cppkiapi.Server
	Beacons        BeaconStore
	CA             renewal.ChainBuilder
	Config         http.HandlerFunc
	Info           http.HandlerFunc
	LogLevel       http.HandlerFunc
	Signer         cstrust.RenewingSigner
	Topology       http.HandlerFunc
	TrustDB        storage.TrustDB
	Healther       Healther

	// nowProvider can be set during tests to control the current time.
	nowProvider func() time.Time
}

// UnpackBeaconUsages extracts the Usage's bits as snake case string constants for the API.
func UnpackBeaconUsages(u beacon.Usage) []string {
	var names []string
	if u&beacon.UsageUpReg != 0 {
		names = append(names, string(UpRegistration))
	}
	if u&beacon.UsageDownReg != 0 {
		names = append(names, string(DownRegistration))
	}
	if u&beacon.UsageCoreReg != 0 {
		names = append(names, string(CoreRegistration))
	}
	if u&beacon.UsageProp != 0 {
		names = append(names, string(Propagation))
	}
	return names
}

// GetBeacons gets the stored in the BeaconDB.
func (s *Server) GetBeacons(w http.ResponseWriter, r *http.Request, params GetBeaconsParams) {
	q := beaconstorage.QueryParams{}
	var errs serrors.List
	if params.StartIsdAs != nil {
		if ia, err := addr.ParseIA(*params.StartIsdAs); err == nil {
			q.StartsAt = []addr.IA{ia}
		} else {
			errs = append(errs, serrors.Wrap("parsing start_isd_as", err))
		}
	}
	if params.Usages != nil {
		var usage beacon.Usage
		for _, usageFlag := range *params.Usages {
			switch usageFlag {
			case CoreRegistration:
				usage |= beacon.UsageCoreReg
			case DownRegistration:
				usage |= beacon.UsageDownReg
			case Propagation:
				usage |= beacon.UsageProp
			case UpRegistration:
				usage |= beacon.UsageUpReg
			default:
				errs = append(errs, serrors.New(
					"unknown value for parameter",
					"usage",
					usageFlag,
				))
			}
		}
		q.Usages = []beacon.Usage{usage}
	}

	if params.IngressInterface != nil {
		if *params.IngressInterface < 0 || *params.IngressInterface > 65535 {
			errs = append(errs, serrors.New(
				"value for parameter out of range",
				"ingress_interface",
				*params.IngressInterface,
			))
		}
		q.IngressInterfaces = []uint16{uint16(*params.IngressInterface)}
	}
	switch {
	case (params.All != nil) && *params.All:
		q.ValidAt = time.Time{}
	case params.ValidAt != nil:
		q.ValidAt = *params.ValidAt
	default:
		q.ValidAt = time.Now()
	}
	sortFn, err := sortFactory(params.Sort)
	if err != nil {
		errs = append(errs, err)
	}

	if err := errs.ToError(); err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusBadRequest,
			Title:  "malformed query parameters",
			Type:   api.StringRef(api.BadRequest),
		})
		return
	}
	results, err := s.Beacons.GetBeacons(r.Context(), &q)
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "error getting beacons",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}

	rep := make([]*Beacon, 0, len(results))
	for _, result := range results {
		s := result.Beacon.Segment
		var usage BeaconUsages
		for _, name := range UnpackBeaconUsages(result.Usage) {
			usage = append(usage, BeaconUsage(name))
		}
		var hops []Hop
		for i, as := range s.ASEntries {
			if i != 0 {
				hops = append(hops, Hop{
					Interface: int(as.HopEntry.HopField.ConsIngress),
					IsdAs:     as.Local.String(),
				})
			}
			hops = append(hops, Hop{
				Interface: int(as.HopEntry.HopField.ConsEgress),
				IsdAs:     as.Local.String(),
			})
		}
		rep = append(rep, &Beacon{
			Usages:           usage,
			IngressInterface: int(result.Beacon.InIfID),
			Id:               segapi.SegID(s),
			LastUpdated:      result.LastUpdated,
			Timestamp:        s.Info.Timestamp.UTC(),
			Expiration:       s.MinExpiry().UTC(),
			Hops:             hops,
		})
	}
	// Sort the results.
	sorter := sortFn(rep)
	if params.Desc != nil && *params.Desc {
		sorter = sort.Reverse(sorter)
	}
	sort.Sort(sorter)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	if err := enc.Encode(map[string][]*Beacon{"beacons": rep}); err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to marshal response",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
}

type sortWrapper struct {
	beacons []*Beacon
	less    func(a, b *Beacon) bool
}

func (w sortWrapper) Len() int           { return len(w.beacons) }
func (w sortWrapper) Less(i, j int) bool { return w.less(w.beacons[i], w.beacons[j]) }
func (w sortWrapper) Swap(i, j int)      { w.beacons[i], w.beacons[j] = w.beacons[j], w.beacons[i] }

// sortFactory returns a function that wraps a list of beacons in a sortWrapper.
// The returned sortWrapper implements the sort.Interface with a less function that depends on
// the provided sortParam.
func sortFactory(sortParam *GetBeaconsParamsSort) (func(b []*Beacon) sort.Interface, error) {
	by := "last_updated"
	if sortParam != nil {
		by = string(*sortParam)
	}
	var less func(a, b *Beacon) bool
	switch by {
	case "expiration_time":
		less = func(a, b *Beacon) bool {
			return a.Expiration.Before(b.Expiration)
		}
	case "info_time":
		less = func(a, b *Beacon) bool {
			return a.Timestamp.Before(b.Timestamp)
		}
	case "start_isd_as":
		less = func(a, b *Beacon) bool {
			if len(a.Hops) == 0 || len(b.Hops) == 0 {
				return len(a.Hops) < len(b.Hops)
			}
			return a.Hops[0].IsdAs < b.Hops[0].IsdAs
		}
	case "last_updated":
		less = func(a, b *Beacon) bool {
			return a.LastUpdated.Before(b.LastUpdated)
		}
	case "ingress_interface":
		less = func(a, b *Beacon) bool {
			return a.IngressInterface < b.IngressInterface
		}
	default:
		return nil, serrors.New("unknown query parameter", "sort", by)
	}
	return func(b []*Beacon) sort.Interface {
		return sortWrapper{
			beacons: b,
			less:    less,
		}
	}, nil
}
func (s *Server) GetBeacon(w http.ResponseWriter, r *http.Request, segmentId SegmentID) {
	id, err := hex.DecodeString(segmentId)
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusBadRequest,
			Title:  "error decoding segment id",
			Type:   api.StringRef(api.BadRequest),
		})
		return
	}
	q := beaconstorage.QueryParams{
		SegIDs: [][]byte{id},
	}
	results, err := s.Beacons.GetBeacons(r.Context(), &q)
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "error getting beacons",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	if len(results) == 0 {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(fmt.Sprintf(
				"no beacon matched provided segment ID: %s",
				segmentId,
			)),
			Status: http.StatusBadRequest,
			Title:  "malformed query parameter",
			Type:   api.StringRef(api.BadRequest),
		})
		return
	}
	if len(results) > 1 {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(fmt.Sprintf(
				"%d beacons matched provided segment ID: %s",
				len(results),
				segmentId,
			)),
			Status: http.StatusBadRequest,
			Title:  "malformed query parameter",
			Type:   api.StringRef(api.BadRequest),
		})
		return
	}
	seg := results[0].Beacon.Segment
	var usage BeaconUsages
	for _, name := range UnpackBeaconUsages(results[0].Usage) {
		usage = append(usage, BeaconUsage(name))
	}
	var hops []Hop
	for i, as := range seg.ASEntries {
		if i != 0 {
			hops = append(hops, Hop{
				Interface: int(as.HopEntry.HopField.ConsIngress),
				IsdAs:     as.Local.String(),
			})
		}
		hops = append(hops, Hop{
			Interface: int(as.HopEntry.HopField.ConsEgress),
			IsdAs:     as.Local.String(),
		})
	}
	res := map[string]Beacon{
		"beacon": {
			Usages:           usage,
			IngressInterface: int(results[0].Beacon.InIfID),
			Id:               segapi.SegID(seg),
			LastUpdated:      results[0].LastUpdated,
			Timestamp:        seg.Info.Timestamp.UTC(),
			Expiration:       seg.MinExpiry().UTC(),
			Hops:             hops,
		},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	if err := enc.Encode(res); err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to marshal response",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
}

func (s *Server) DeleteBeacon(w http.ResponseWriter, r *http.Request, segmentId SegmentID) {
	if segmentId == "" {
		ErrorResponse(w, Problem{
			Status: http.StatusBadRequest,
			Title:  "segment ID is required",
			Type:   api.StringRef(api.BadRequest),
		})
		return
	}
	if err := s.Beacons.DeleteBeacon(r.Context(), segmentId); err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to delete beacon",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) GetBeaconBlob(w http.ResponseWriter, r *http.Request, segmentId SegmentID) {
	w.Header().Set("Content-Type", "application/x-pem-file")

	id, err := hex.DecodeString(segmentId)
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusBadRequest,
			Title:  "error decoding segment id",
			Type:   api.StringRef(api.BadRequest),
		})
		return
	}
	q := beaconstorage.QueryParams{
		SegIDs: [][]byte{id},
	}
	results, err := s.Beacons.GetBeacons(r.Context(), &q)
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "error getting beacons",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	if len(results) == 0 {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(fmt.Sprintf(
				"no beacon matched provided segment ID: %s",
				segmentId,
			)),
			Status: http.StatusBadRequest,
			Title:  "malformed query parameter",
			Type:   api.StringRef(api.BadRequest),
		})
		return
	}
	if len(results) > 1 {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(fmt.Sprintf(
				"%d beacons matched provided segment ID: %s",
				len(results),
				segmentId,
			)),
			Status: http.StatusBadRequest,
			Title:  "malformed query parameter",
			Type:   api.StringRef(api.BadRequest),
		})
		return
	}
	var buf bytes.Buffer
	segment := results[0].Beacon.Segment
	bytes, err := proto.Marshal(seg.PathSegmentToPB(segment))
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to marshal beacon",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	b := &pem.Block{
		Type:  "PATH SEGMENT",
		Bytes: bytes,
	}
	if err := pem.Encode(&buf, b); err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to marshal response",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// GetSegments gets the stored in the PathDB.
func (s *Server) GetSegments(w http.ResponseWriter,
	r *http.Request, params GetSegmentsParams) {
	p := segapi.GetSegmentsParams{
		StartIsdAs: params.StartIsdAs,
		EndIsdAs:   params.EndIsdAs,
	}
	s.SegmentsServer.GetSegments(w, r, p)
}

func (s *Server) GetSegment(w http.ResponseWriter, r *http.Request, id SegmentID) {
	s.SegmentsServer.GetSegment(w, r, id)
}

func (s *Server) DeleteSegment(w http.ResponseWriter, r *http.Request, id SegmentID) {
	s.SegmentsServer.DeleteSegment(w, r, id)
}

func (s *Server) GetSegmentBlob(w http.ResponseWriter, r *http.Request, id SegmentID) {
	s.SegmentsServer.GetSegmentBlob(w, r, id)
}

// GetCertificates lists the certificate chains.
func (s *Server) GetCertificates(w http.ResponseWriter,
	r *http.Request, params GetCertificatesParams) {
	cppkiParams := cppkiapi.GetCertificatesParams{
		IsdAs:   params.IsdAs,
		ValidAt: params.ValidAt,
		All:     params.All,
	}
	s.CPPKIServer.GetCertificates(w, r, cppkiParams)
}

// GetCertificate lists the certificate chain for a given ChainID.
func (s *Server) GetCertificate(w http.ResponseWriter, r *http.Request, chainID ChainID) {
	s.CPPKIServer.GetCertificate(w, r, chainID)
}

// GetCertificateBlob generates a certificate chain blob response encoded as PEM
// for a given chainId.
func (s *Server) GetCertificateBlob(w http.ResponseWriter, r *http.Request, chainID ChainID) {
	s.CPPKIServer.GetCertificateBlob(w, r, chainID)
}

// GetCa gets the CA info.
func (s *Server) GetCa(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.CA.PolicyGen == nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef("This instance is not configured with CA capability"),
			Status: http.StatusNotImplemented,
			Title:  "Not a CA",
			Type:   api.StringRef(api.NotImplemented),
		})
		return
	}

	p, err := s.CA.PolicyGen.Generate(r.Context())
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "No active signer",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	ia, err := cppki.ExtractIA(p.Certificate.Subject)
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "Unable to extract ISD-AS",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	rep := CA{
		CertValidity: Validity{
			NotAfter:  p.Certificate.NotAfter,
			NotBefore: p.Certificate.NotBefore,
		},
		Policy: Policy{
			ChainLifetime: p.Validity.String(),
		},
		Subject: Subject{
			IsdAs: ia.String(),
		},
		SubjectKeyId: fmt.Sprintf("% X", p.Certificate.SubjectKeyId),
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	if err := enc.Encode(rep); err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to marshal response",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
}

// GetTrcs gets the trcs specified by it's params.
func (s *Server) GetTrcs(
	w http.ResponseWriter,
	r *http.Request,
	params GetTrcsParams, // nolint - name from published API
) {
	cppkiParams := cppkiapi.GetTrcsParams{ // nolint - name from published API
		Isd: params.Isd,
		All: params.All,
	}
	s.CPPKIServer.GetTrcs(w, r, cppkiParams) // nolint - name from published API
}

// GetTrc gets the trc specified by it's isd base and serial.
func (s *Server) GetTrc(w http.ResponseWriter, r *http.Request, isd int, base int, serial int) {
	s.CPPKIServer.GetTrc(w, r, isd, base, serial) // nolint - name from published API
}

// GetTrcBlob gets the trc encoded pem blob.
func (s *Server) GetTrcBlob(w http.ResponseWriter, r *http.Request, isd int, base int, serial int) {
	s.CPPKIServer.GetTrcBlob(w, r, isd, base, serial) // nolint - name from published API
}

// GetConfig is an indirection to the http handler.
func (s *Server) GetConfig(w http.ResponseWriter, r *http.Request) {
	s.Config(w, r)
}

// GetInfo is an indirection to the http handler.
func (s *Server) GetInfo(w http.ResponseWriter, r *http.Request) {
	s.Info(w, r)
}

// GetLogLevel is an indirection to the http handler.
func (s *Server) GetLogLevel(w http.ResponseWriter, r *http.Request) {
	s.LogLevel(w, r)
}

// SetLogLevel is an indirection to the http handler.
func (s *Server) SetLogLevel(w http.ResponseWriter, r *http.Request) {
	s.LogLevel(w, r)
}

// GetSigner generates the singer response content.
func (s *Server) GetSigner(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	signers, err := s.Signer.SignerGen.Generate(r.Context())
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "Unable to get signer",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	now := s.now()
	p, err := trust.LastExpiring(signers, cppki.Validity{
		NotBefore: now,
		NotAfter:  now,
	})
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "No signer currently valid",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	rep := Signer{
		AsCertificate: Certificate{
			DistinguishedName: p.Subject.String(),
			IsdAs:             p.IA.String(),
			SubjectKeyAlgo:    p.Algorithm.String(),
			SubjectKeyId:      fmt.Sprintf("% X", p.SubjectKeyID),
			Validity: Validity{
				NotAfter:  p.ChainValidity.NotAfter,
				NotBefore: p.ChainValidity.NotBefore,
			},
		},
		Expiration: p.Expiration,
		TrcId: TRCID{ // nolint - name from published API
			BaseNumber:   int(p.TRCID.Base),
			Isd:          int(p.TRCID.ISD),
			SerialNumber: int(p.TRCID.Serial),
		},
		TrcInGracePeriod: p.InGrace, // nolint - name from published API
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	if err := enc.Encode(rep); err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to marshal response",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
}

// GetSignerChain generates a certificate chain blob response encoded as PEM.
func (s *Server) GetSignerChain(w http.ResponseWriter, r *http.Request) {
	signers, err := s.Signer.SignerGen.Generate(r.Context())
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to get signer",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	now := s.now()
	p, err := trust.LastExpiring(signers, cppki.Validity{
		NotBefore: now,
		NotAfter:  now,
	})
	if err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "no signer currently valid",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	var buf bytes.Buffer
	if len(p.Chain) == 0 {
		ErrorResponse(w, Problem{
			Status: http.StatusInternalServerError,
			Title:  "no certificates available",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
	for _, cert := range p.Chain {
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
			ErrorResponse(w, Problem{
				Detail: api.StringRef(err.Error()),
				Status: http.StatusInternalServerError,
				Title:  "unable to marshal response",
				Type:   api.StringRef(api.InternalError),
			})
			return
		}
	}
	_, _ = w.Write(buf.Bytes())
}

// GetTopology is an indirection to the http handler.
func (s *Server) GetTopology(w http.ResponseWriter, r *http.Request) {
	s.Topology(w, r)
}

func (s *Server) GetHealth(w http.ResponseWriter, r *http.Request) {

	var checks []Check

	signerHealth := s.Healther.GetSignerHealth(r.Context())
	signerCheck := Check{
		Status: Passing,
		Name:   "valid signer available",
	}
	switch {
	case signerHealth.SignerMissing:
		signerCheck.Status = Failing
		if signerHealth.SignerMissingDetail != "" {
			signerCheck.Detail = api.StringRef(signerHealth.SignerMissingDetail)
		}
	case time.Until(signerHealth.Expiration) <= 0:
		signerCheck.Status = Failing
		signerCheck.Detail = api.StringRef("signer certificate has expired")
		signerCheck.Data = CheckData{
			"expires_at": signerHealth.Expiration.Format(time.RFC3339),
		}
	case signerHealth.InGrace:
		signerCheck.Status = Degraded
		signerCheck.Data = CheckData{
			"expires_at": signerHealth.Expiration.Format(time.RFC3339),
			"in_grace":   true,
		}
		signerCheck.Detail = api.StringRef(`signer certificate is authenticated
		by TRC in grace period`)
	case time.Until(signerHealth.Expiration) < 6*time.Hour:
		signerCheck.Status = Degraded
		signerCheck.Data = CheckData{
			"expires_at": signerHealth.Expiration.Format(time.RFC3339),
		}
		signerCheck.Detail = api.StringRef("signer certificate is close to expiration")
	default:
		signerCheck.Data = CheckData{
			"expires_at": signerHealth.Expiration.Format(time.RFC3339),
		}
	}
	checks = append(checks, signerCheck)

	trcCheck := Check{
		Status: Failing,
		Name:   "TRC for local ISD available",
	}
	trcHealthData := s.Healther.GetTRCHealth(r.Context())
	if trcHealthData.TRCNotFoundDetail != "" {
		trcCheck.Detail = api.StringRef(trcHealthData.TRCNotFoundDetail)
	}
	if !trcHealthData.TRCNotFound {
		trcCheck.Status = Passing
		trcCheck.Data = CheckData{
			"base_number":   trcHealthData.TRCID.Base,
			"serial_number": trcHealthData.TRCID.Serial,
			"isd":           trcHealthData.TRCID.ISD,
		}
	}
	checks = append(checks, trcCheck)

	if status, ok := s.Healther.GetCAHealth(r.Context()); ok {
		caCheck := Check{
			Status: Degraded,
			Name:   "CPPKI CA Connection",
		}
		if status == Available {
			caCheck.Status = Passing
		}
		caCheck.Data = CheckData{
			"status": status,
		}
		checks = append(checks, caCheck)
	}
	rep := HealthResponse{
		Health: Health{
			Status: Status(healthapi.AggregateHealthStatus(
				func() []healthapi.Status {
					statuses := make([]healthapi.Status, 0, len(checks))
					for _, c := range checks {
						statuses = append(statuses, healthapi.Status(c.Status))
					}
					return statuses
				}()),
			),
			Checks: checks,
		},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	if err := enc.Encode(rep); err != nil {
		ErrorResponse(w, Problem{
			Detail: api.StringRef(err.Error()),
			Status: http.StatusInternalServerError,
			Title:  "unable to marshal response",
			Type:   api.StringRef(api.InternalError),
		})
		return
	}
}

func (s *Server) now() time.Time {
	if s.nowProvider != nil {
		return s.nowProvider()
	}
	return time.Now()
}

// Error creates an detailed error response.
func ErrorResponse(w http.ResponseWriter, p Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	// no point in catching error here, there is nothing we can do about it anymore.
	_ = enc.Encode(p)
}
