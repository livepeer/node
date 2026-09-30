// Package wire preserves the retained fields and field numbers of the
// legacy unversioned remote-signer Protobuf envelopes. Unknown fields are
// skipped so current Go and Python runner clients can send their full messages.
package wire

import (
	"errors"
	"math"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

const maxEnvelope = 1 << 20

type PriceInfo struct {
	PricePerUnit  int64
	UnitsPerPrice int64 // Legacy field name: pixelsPerUnit; live/fixed use one billable unit.
}

type ExpirationParams struct {
	CreationRound          int64
	CreationRoundBlockHash []byte
}

type TicketParams struct {
	Recipient, FaceValue, WinProb, RecipientRandHash, Seed, ExpirationBlock []byte
	Expiration                                                              ExpirationParams
}

type AuthToken struct {
	Token      []byte
	SessionID  string
	Expiration int64
}

type OrchestratorInfo struct {
	Transcoder   string
	TicketParams TicketParams
	Price        PriceInfo
	Address      []byte
	Auth         AuthToken
}

type TicketSenderParams struct {
	SenderNonce uint32
	Sig         []byte
}

type Payment struct {
	TicketParams  TicketParams
	Sender        []byte
	Expiration    ExpirationParams
	SenderParams  []TicketSenderParams
	ExpectedPrice PriceInfo
}

type SegData struct {
	ManifestID, Hash, Signature []byte
	Auth                        AuthToken
}

func each(data []byte, fn func(protowire.Number, protowire.Type, []byte, uint64) error) error {
	if len(data) > maxEnvelope {
		return errors.New("protobuf envelope exceeds 1 MiB")
	}
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 || number > protowire.MaxValidNumber {
			return errors.New("invalid Protobuf tag")
		}
		data = data[n:]
		var value []byte
		var integer uint64
		switch wireType {
		case protowire.BytesType:
			var consumed int
			value, consumed = protowire.ConsumeBytes(data)
			n = consumed
		case protowire.VarintType:
			integer, n = protowire.ConsumeVarint(data)
		default:
			n = protowire.ConsumeFieldValue(number, wireType, data)
		}
		if n < 0 {
			return errors.New("invalid Protobuf value")
		}
		if err := fn(number, wireType, value, integer); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func bytesField(number protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, number, protowire.BytesType), value)
}
func intField(number protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, number, protowire.VarintType), value)
}
func DecodePrice(data []byte) (PriceInfo, error) {
	var p PriceInfo
	err := p.decode(data)
	return p, err
}

func (p *PriceInfo) decode(data []byte) error {
	return each(data, func(n protowire.Number, typ protowire.Type, _ []byte, v uint64) error {
		if n == 1 || n == 2 {
			if typ != protowire.VarintType {
				return nil
			}
			if n == 1 {
				p.PricePerUnit = int64(v)
			} else {
				p.UnitsPerPrice = int64(v)
			}
		}
		return nil
	})
}
func EncodePrice(p PriceInfo) []byte {
	return append(intField(1, uint64(p.PricePerUnit)), intField(2, uint64(p.UnitsPerPrice))...)
}

func DecodeExpiration(data []byte) (ExpirationParams, error) {
	var e ExpirationParams
	err := e.decode(data)
	return e, err
}

func (e *ExpirationParams) decode(data []byte) error {
	return each(data, func(n protowire.Number, typ protowire.Type, b []byte, v uint64) error {
		switch n {
		case 1:
			if typ != protowire.VarintType {
				return nil
			}
			e.CreationRound = int64(v)
		case 2:
			if typ != protowire.BytesType {
				return nil
			}
			e.CreationRoundBlockHash = append([]byte(nil), b...)
		}
		return nil
	})
}
func EncodeExpiration(e ExpirationParams) []byte {
	return append(intField(1, uint64(e.CreationRound)), bytesField(2, e.CreationRoundBlockHash)...)
}

func DecodeTicketParams(data []byte) (TicketParams, error) {
	var p TicketParams
	err := p.decode(data)
	return p, err
}

func (p *TicketParams) decode(data []byte) error {
	return each(data, func(n protowire.Number, typ protowire.Type, b []byte, _ uint64) error {
		if n < 1 || n > 7 {
			return nil
		}
		if typ != protowire.BytesType {
			return nil
		}
		switch n {
		case 1:
			p.Recipient = append([]byte(nil), b...)
		case 2:
			p.FaceValue = append([]byte(nil), b...)
		case 3:
			p.WinProb = append([]byte(nil), b...)
		case 4:
			p.RecipientRandHash = append([]byte(nil), b...)
		case 5:
			p.Seed = append([]byte(nil), b...)
		case 6:
			p.ExpirationBlock = append([]byte(nil), b...)
		case 7:
			return p.Expiration.decode(b)
		}
		return nil
	})
}
func EncodeTicketParams(p TicketParams) []byte {
	var out []byte
	for n, b := range [][]byte{p.Recipient, p.FaceValue, p.WinProb, p.RecipientRandHash, p.Seed, p.ExpirationBlock} {
		out = append(out, bytesField(protowire.Number(n+1), b)...)
	}
	return append(out, bytesField(7, EncodeExpiration(p.Expiration))...)
}

func DecodeAuthToken(data []byte) (AuthToken, error) {
	var a AuthToken
	err := a.decode(data)
	return a, err
}

func (a *AuthToken) decode(data []byte) error {
	return each(data, func(n protowire.Number, typ protowire.Type, b []byte, v uint64) error {
		switch n {
		case 1, 2:
			if typ != protowire.BytesType {
				return nil
			}
			if n == 1 {
				a.Token = append([]byte(nil), b...)
			} else {
				if !utf8.Valid(b) {
					return errors.New("invalid Protobuf UTF-8")
				}
				a.SessionID = string(b)
			}
		case 3:
			if typ != protowire.VarintType {
				return nil
			}
			a.Expiration = int64(v)
		}
		return nil
	})
}
func EncodeAuthToken(a AuthToken) []byte {
	out := append(bytesField(1, a.Token), bytesField(2, []byte(a.SessionID))...)
	return append(out, intField(3, uint64(a.Expiration))...)
}

func DecodeOrchestratorInfo(data []byte) (OrchestratorInfo, error) {
	var o OrchestratorInfo
	err := o.decode(data)
	return o, err
}

func (o *OrchestratorInfo) decode(data []byte) error {
	return each(data, func(n protowire.Number, typ protowire.Type, b []byte, _ uint64) error {
		if n < 1 || n > 6 || n == 5 {
			return nil
		}
		if typ != protowire.BytesType {
			return nil
		}
		switch n {
		case 1:
			if !utf8.Valid(b) {
				return errors.New("invalid Protobuf UTF-8")
			}
			o.Transcoder = string(b)
		case 2:
			return o.TicketParams.decode(b)
		case 3:
			return o.Price.decode(b)
		case 4:
			o.Address = append([]byte(nil), b...)
		case 6:
			return o.Auth.decode(b)
		}
		return nil
	})
}
func EncodeOrchestratorInfo(o OrchestratorInfo) []byte {
	var out []byte
	out = append(out, bytesField(1, []byte(o.Transcoder))...)
	out = append(out, bytesField(2, EncodeTicketParams(o.TicketParams))...)
	out = append(out, bytesField(3, EncodePrice(o.Price))...)
	out = append(out, bytesField(4, o.Address)...)
	return append(out, bytesField(6, EncodeAuthToken(o.Auth))...)
}

func DecodePayment(data []byte) (Payment, error) {
	var p Payment
	err := p.decode(data)
	return p, err
}

func (p *Payment) decode(data []byte) error {
	return each(data, func(n protowire.Number, typ protowire.Type, b []byte, _ uint64) error {
		if n < 1 || n > 5 {
			return nil
		}
		if typ != protowire.BytesType {
			return nil
		}
		switch n {
		case 1:
			return p.TicketParams.decode(b)
		case 2:
			p.Sender = append([]byte(nil), b...)
		case 3:
			return p.Expiration.decode(b)
		case 4:
			var sp TicketSenderParams
			err := each(b, func(k protowire.Number, typ protowire.Type, b []byte, v uint64) error {
				switch k {
				case 1:
					if typ != protowire.VarintType {
						return nil
					}
					if v > math.MaxUint32 {
						return errors.New("sender nonce exceeds uint32")
					}
					sp.SenderNonce = uint32(v)
				case 2:
					if typ != protowire.BytesType {
						return nil
					}
					sp.Sig = append([]byte(nil), b...)
				}
				return nil
			})
			if err != nil {
				return err
			}
			p.SenderParams = append(p.SenderParams, sp)
		case 5:
			return p.ExpectedPrice.decode(b)
		}
		return nil
	})
}
func EncodePayment(p Payment) []byte {
	var out []byte
	out = append(out, bytesField(1, EncodeTicketParams(p.TicketParams))...)
	out = append(out, bytesField(2, p.Sender)...)
	out = append(out, bytesField(3, EncodeExpiration(p.Expiration))...)
	for _, sp := range p.SenderParams {
		b := append(intField(1, uint64(sp.SenderNonce)), bytesField(2, sp.Sig)...)
		out = append(out, bytesField(4, b)...)
	}
	return append(out, bytesField(5, EncodePrice(p.ExpectedPrice))...)
}

func DecodeSegData(data []byte) (SegData, error) {
	var s SegData
	err := s.decode(data)
	return s, err
}

func (s *SegData) decode(data []byte) error {
	return each(data, func(n protowire.Number, typ protowire.Type, b []byte, _ uint64) error {
		if n != 1 && n != 3 && n != 5 && n != 8 {
			return nil
		}
		if typ != protowire.BytesType {
			return nil
		}
		switch n {
		case 1:
			s.ManifestID = append([]byte(nil), b...)
		case 3:
			s.Hash = append([]byte(nil), b...)
		case 5:
			s.Signature = append([]byte(nil), b...)
		case 8:
			return s.Auth.decode(b)
		}
		return nil
	})
}
func EncodeSegData(s SegData) []byte {
	var out []byte
	out = append(out, bytesField(1, s.ManifestID)...)
	out = append(out, bytesField(3, s.Hash)...)
	out = append(out, bytesField(5, s.Signature)...)
	return append(out, bytesField(8, EncodeAuthToken(s.Auth))...)
}
