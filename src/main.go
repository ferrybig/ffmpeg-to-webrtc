//go:build !js
// +build !js

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

const (
	h264FrameDuration = time.Millisecond * 33
)

var globalConnectionId = 0

func setupConnection(browserOffer string) (string, error) {
	globalConnectionId++
	connectionId := globalConnectionId
	fmt.Printf("[%d] Starting new session...\n", connectionId)
	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		return "", err
	}

	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(context.Background())

	// Create a video track
	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if videoTrackErr != nil {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
		}
		iceConnectedCtxCancel()
		return "", videoTrackErr
	}

	rtpSender, videoTrackErr := peerConnection.AddTrack(videoTrack)
	if videoTrackErr != nil {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
		}
		iceConnectedCtxCancel()
		return "", videoTrackErr
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	go func() {
		dataPipe, err := RunCommand("ffmpeg", os.Args[1:]...)

		if err != nil {
			fmt.Printf("[%d] datapipe err: %v\n", connectionId, err)
			if cErr := peerConnection.Close(); cErr != nil {
				fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
			}
			return
		}

		h264, h264Err := h264reader.NewReader(dataPipe)
		if h264Err != nil {
			fmt.Printf("[%d] h264Err: %v\n", connectionId, h264Err)
			if cErr := peerConnection.Close(); cErr != nil {
				fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
			}
			return
		}

		// Wait for connection established
		<-iceConnectedCtx.Done()

		// Send our video file frame at a time. Pace our sending so we send it at the same speed it should be played back as.
		// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
		//
		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		spsAndPpsCache := []byte{}
		ticker := time.NewTicker(h264FrameDuration)
		for ; true; <-ticker.C {
			nal, h264Err := h264.NextNAL()
			if h264Err == io.EOF {
				fmt.Printf("[%d] All video frames parsed and sent\n", connectionId)
				if cErr := peerConnection.Close(); cErr != nil {
					fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
				}
				if cErr := dataPipe.Close(); cErr != nil {
					fmt.Printf("[%d] cannot close dataPipe: %v\n", connectionId, cErr)
				}
				return
			}
			if h264Err != nil {
				fmt.Printf("[%d] h264Err: %v\n", connectionId, h264Err)
				if cErr := peerConnection.Close(); cErr != nil {
					fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
				}
				if cErr := dataPipe.Close(); cErr != nil {
					fmt.Printf("[%d] cannot close dataPipe: %v\n", connectionId, cErr)
				}
				return
			}

			nal.Data = append([]byte{0x00, 0x00, 0x00, 0x01}, nal.Data...)

			if nal.UnitType == h264reader.NalUnitTypeSPS || nal.UnitType == h264reader.NalUnitTypePPS {
				spsAndPpsCache = append(spsAndPpsCache, nal.Data...)
				continue
			} else if nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr {
				nal.Data = append(spsAndPpsCache, nal.Data...)
				spsAndPpsCache = []byte{}
			}

			if h264Err = videoTrack.WriteSample(media.Sample{Data: nal.Data, Duration: time.Second}); h264Err != nil {
				fmt.Printf("[%d] h264Err: %v\n", connectionId, h264Err)
				if cErr := peerConnection.Close(); cErr != nil {
					fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
				}
				if cErr := dataPipe.Close(); cErr != nil {
					fmt.Printf("[%d] cannot close dataPipe: %v\n", connectionId, cErr)
				}
				return
			}
		}
	}()

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("[%d] Connection State has changed %s\n", connectionId, connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			iceConnectedCtxCancel()
		}
	})

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("[%d] Peer Connection State has changed: %s\n", connectionId, s.String())

		if s == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure. It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			fmt.Printf("[%d] Exiting...", connectionId)

			if cErr := peerConnection.Close(); cErr != nil {
				fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
			}
		}
	})

	offer := webrtc.SessionDescription{}
	offer.Type = webrtc.SDPTypeOffer
	offer.SDP = browserOffer

	fmt.Printf("[%d] Reading offer...\n%s\n", connectionId, browserOffer)
	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
		}
		return "", err
	}

	fmt.Printf("[%d] Creating answer...\n", connectionId)
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
		}
		return "", err
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	fmt.Printf("[%d] Setting local description...\n", connectionId)
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("[%d] cannot close peerConnection: %v\n", connectionId, cErr)
		}
		return "", err
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	fmt.Printf("[%d] Sending local description...\n", connectionId)
	sdp := *peerConnection.LocalDescription()
	return sdp.SDP, nil
}

func main() {
	fmt.Printf("Starting...\n")
	r := mux.NewRouter()
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("content-type") == "application/sdp" {
			buf := new(strings.Builder)
			if _, err := io.Copy(buf, r.Body); err != nil {
				http.Error(w, "Error1: "+err.Error(), http.StatusInternalServerError)
				return
			}

			sdpOffer := buf.String()
			sdpAnswer, err := setupConnection(sdpOffer)
			if err != nil {
				http.Error(w, "Error2: "+err.Error(), http.StatusInternalServerError)
				return
			}
			fmt.Printf(sdpAnswer)
			w.Header().Set("Content-Type", "application/sdp")
			w.Write([]byte(sdpAnswer))
			return
		}
		http.Error(w, "Unaceptable", http.StatusUnsupportedMediaType)
	}).Methods("POST")

	fmt.Printf("Listening on: http://[::]:5050/\n")
	http.ListenAndServe("[::]:5050", r)

}
