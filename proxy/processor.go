package proxy

import (
	"errors"
	"fmt"
	"github.com/grepplabs/kafka-proxy/proxy/protocol"
	"io"
	"time"
)

const (
	inFlightRequestSendTimeout    = 5 * time.Second
	inFlightRequestReceiveTimeout = 5 * time.Second
)

type ProcessorConfig struct {
	InFlightRequests      int
	NetAddressMappingFunc protocol.NetAddressMappingFunc
}

type processor struct {
	inFlightRequestsChannel chan protocol.RequestKeyVersion
	netAddressMappingFunc   protocol.NetAddressMappingFunc
}

func newProcessor(cfg ProcessorConfig) *processor {
	inFlightRequests := cfg.InFlightRequests
	if inFlightRequests < 1 {
		inFlightRequests = 1
	}
	return &processor{
		inFlightRequestsChannel: make(chan protocol.RequestKeyVersion, inFlightRequests),
		netAddressMappingFunc:   cfg.NetAddressMappingFunc,
	}
}

func (p *processor) RequestsLoop(dst io.Writer, src io.Reader) (readErr bool, err error) {
	return requestsLoop(dst, src, p.inFlightRequestsChannel)
}

func (p *processor) ResponsesLoop(dst io.Writer, src io.Reader) (readErr bool, err error) {
	return responsesLoop(dst, src, p.inFlightRequestsChannel, p.netAddressMappingFunc)
}

func requestsLoop(dst io.Writer, src io.Reader, inFlightRequests chan<- protocol.RequestKeyVersion) (readErr bool, err error) {
	keyVersionBuf := make([]byte, 8) // Size => int32 + ApiKey => int16 + ApiVersion => int16

	buf := make([]byte, 4096)

	for {
		// log.Println("Await Kafka request")

		if _, err = io.ReadFull(src, keyVersionBuf); err != nil {
			return true, err
		}

		requestKeyVersion := &protocol.RequestKeyVersion{}
		if err = protocol.Decode(keyVersionBuf, requestKeyVersion); err != nil {
			return true, err
		}
		// log.Printf("Kafka request length %v, key %v, version %v", requestKeyVersion.Length, requestKeyVersion.ApiKey, requestKeyVersion.ApiVersion)

		// send inFlightRequest to channel before myCopyN to prevent race condition in proxyResponses
		if err = sendRequestKeyVersion(inFlightRequests, inFlightRequestSendTimeout, requestKeyVersion); err != nil {
			return true, err
		}

		// write - send to broker
		if _, err = dst.Write(keyVersionBuf); err != nil {
			return false, err
		}
		// 4 bytes were written as keyVersionBuf (ApiKey, ApiVersion)
		if readErr, err = myCopyN(dst, src, int64(requestKeyVersion.Length-4), buf); err != nil {
			return readErr, err
		}
	}
}

func responsesLoop(dst io.Writer, src io.Reader, inFlightRequests <-chan protocol.RequestKeyVersion, netAddressMappingFunc protocol.NetAddressMappingFunc) (readErr bool, err error) {
	responseHeaderBuf := make([]byte, 8) // Size => int32, CorrelationId => int32

	buf := make([]byte, 4096)

	for {
		// log.Println("Await Kafka response")

		if _, err = io.ReadFull(src, responseHeaderBuf); err != nil {
			return true, err
		}

		var responseHeader protocol.ResponseHeader
		if err = protocol.Decode(responseHeaderBuf, &responseHeader); err != nil {
			return true, err
		}

		// Read the inFlightRequests channel after header is read. Otherwise the channel would block and socket EOF from remote would not be received.
		requestKeyVersion, err := receiveRequestKeyVersion(inFlightRequests, inFlightRequestReceiveTimeout)
		if err != nil {
			return true, err
		}
		// log.Printf("Kafka response lenght %v for key %v, version %v", responseHeader.Length, requestKeyVersion.ApiKey, requestKeyVersion.ApiVersion)

		responseModifier, err := protocol.GetResponseModifier(requestKeyVersion.ApiKey, requestKeyVersion.ApiVersion, netAddressMappingFunc)
		if err != nil {
			return true, err
		}
		if responseModifier != nil {
			if int32(responseHeader.Length) > protocol.MaxResponseSize {
				return true, protocol.PacketDecodingError{Info: fmt.Sprintf("message of length %d too large", responseHeader.Length)}
			}
			resp := make([]byte, int(responseHeader.Length-4))
			if _, err = io.ReadFull(src, resp); err != nil {
				return true, err
			}
			newResponseBuf, err := responseModifier.Apply(resp)
			if err != nil {
				return true, err
			}
			// add 4 bytes (CorrelationId) to the length
			newHeaderBuf, err := protocol.Encode(&protocol.ResponseHeader{Length: int32(len(newResponseBuf) + 4), CorrelationID: responseHeader.CorrelationID})
			if err != nil {
				return true, err
			}
			if _, err := dst.Write(newHeaderBuf); err != nil {
				return false, err
			}
			if _, err := dst.Write(newResponseBuf); err != nil {
				return false, err
			}
		} else {
			// write - send to local
			if _, err := dst.Write(responseHeaderBuf); err != nil {
				return false, err
			}
			// 4 bytes were written as responseHeaderBuf (CorrelationId)
			if readErr, err = myCopyN(dst, src, int64(responseHeader.Length-4), buf); err != nil {
				return readErr, err
			}
		}
	}
}

func sendRequestKeyVersion(inFlightRequests chan<- protocol.RequestKeyVersion, timeout time.Duration, request *protocol.RequestKeyVersion) error {
	select {
	case inFlightRequests <- *request:
	default:
		// timer.Stop() will be invoked only after sendRequestKeyVersion is finished (not after select default) !
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case inFlightRequests <- *request:
		case <-timer.C:
			return errors.New("in flight request buffer is full")
		}
	}
	return nil
}

func receiveRequestKeyVersion(inFlightRequests <-chan protocol.RequestKeyVersion, timeout time.Duration) (*protocol.RequestKeyVersion, error) {
	var request protocol.RequestKeyVersion
	select {
	case request = <-inFlightRequests:
	default:
		// timer.Stop() will be invoked only after receiveRequestKeyVersion is finished (not after select default) !
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case request = <-inFlightRequests:
		case <-timer.C:
			return nil, errors.New("in flight request is missing")
		}
	}
	return &request, nil
}