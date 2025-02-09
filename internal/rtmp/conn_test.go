package rtmp

import (
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/aac"
	nh264 "github.com/notedit/rtmp/codec/h264"
	"github.com/notedit/rtmp/format/flv/flvio"
	"github.com/stretchr/testify/require"

	"github.com/aler9/rtsp-simple-server/internal/rtmp/base"
)

func splitPath(u *url.URL) (app, stream string) {
	nu := *u
	nu.ForceQuery = false

	pathsegs := strings.Split(nu.RequestURI(), "/")
	if len(pathsegs) == 2 {
		app = pathsegs[1]
	}
	if len(pathsegs) == 3 {
		app = pathsegs[1]
		stream = pathsegs[2]
	}
	if len(pathsegs) > 3 {
		app = strings.Join(pathsegs[1:3], "/")
		stream = strings.Join(pathsegs[3:], "/")
	}
	return
}

func getTcURL(u string) string {
	ur, err := url.Parse(u)
	if err != nil {
		panic(err)
	}
	app, _ := splitPath(ur)
	nu := *ur
	nu.RawQuery = ""
	nu.Path = "/"
	return nu.String() + app
}

func TestReadTracks(t *testing.T) {
	sps := []byte{
		0x67, 0x64, 0x00, 0x0c, 0xac, 0x3b, 0x50, 0xb0,
		0x4b, 0x42, 0x00, 0x00, 0x03, 0x00, 0x02, 0x00,
		0x00, 0x03, 0x00, 0x3d, 0x08,
	}

	pps := []byte{
		0x68, 0xee, 0x3c, 0x80,
	}

	for _, ca := range []string{
		"standard",
		"metadata without codec id",
		"no metadata",
	} {
		t.Run(ca, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:9121")
			require.NoError(t, err)
			defer ln.Close()

			done := make(chan struct{})

			go func() {
				conn, err := ln.Accept()
				require.NoError(t, err)
				defer conn.Close()

				rconn := NewServerConn(conn)
				err = rconn.ServerHandshake()
				require.NoError(t, err)

				videoTrack, audioTrack, err := rconn.ReadTracks()
				require.NoError(t, err)

				switch ca {
				case "standard":
					videoTrack2, err := gortsplib.NewTrackH264(96, sps, pps, nil)
					require.NoError(t, err)
					require.Equal(t, videoTrack2, videoTrack)

					audioTrack2, err := gortsplib.NewTrackAAC(96, 2, 44100, 2, nil, 13, 3, 3)
					require.NoError(t, err)
					require.Equal(t, audioTrack2, audioTrack)

				case "metadata without codec id":
					videoTrack2, err := gortsplib.NewTrackH264(96, sps, pps, nil)
					require.NoError(t, err)
					require.Equal(t, videoTrack2, videoTrack)

					require.Equal(t, (*gortsplib.TrackAAC)(nil), audioTrack)

				case "no metadata":
					videoTrack2, err := gortsplib.NewTrackH264(96, sps, pps, nil)
					require.NoError(t, err)
					require.Equal(t, videoTrack2, videoTrack)

					require.Equal(t, (*gortsplib.TrackAAC)(nil), audioTrack)
				}

				close(done)
			}()

			conn, err := net.Dial("tcp", "127.0.0.1:9121")
			require.NoError(t, err)
			defer conn.Close()

			// C->S handshake C0
			err = base.HandshakeC0{}.Write(conn)
			require.NoError(t, err)

			// C->S handshake C1
			err = base.HandshakeC1{}.Write(conn)
			require.NoError(t, err)

			// S->C handshake S0
			err = base.HandshakeS0{}.Read(conn)
			require.NoError(t, err)

			// S->C handshake S1+S2
			s1s2 := make([]byte, 1536*2)
			_, err = conn.Read(s1s2)
			require.NoError(t, err)

			// C->S handshake C2
			err = base.HandshakeC2{}.Write(conn, s1s2)
			require.NoError(t, err)

			// C->S connect
			byts := flvio.FillAMF0ValsMalloc([]interface{}{
				"connect",
				1,
				flvio.AMFMap{
					{K: "app", V: "/stream"},
					{K: "flashVer", V: "LNX 9,0,124,2"},
					{K: "tcUrl", V: getTcURL("rtmp://127.0.0.1:9121/stream")},
					{K: "fpad", V: false},
					{K: "capabilities", V: 15},
					{K: "audioCodecs", V: 4071},
					{K: "videoCodecs", V: 252},
					{K: "videoFunction", V: 1},
				},
			})
			err = base.Chunk0{
				ChunkStreamID: 3,
				Typ:           0x14,
				BodyLen:       uint32(len(byts)),
				Body:          byts[:128],
			}.Write(conn)
			require.NoError(t, err)
			err = base.Chunk3{
				ChunkStreamID: 3,
				Body:          byts[128:],
			}.Write(conn)
			require.NoError(t, err)

			// S->C window acknowledgement size
			var c0 base.Chunk0
			err = c0.Read(conn, 128)
			require.NoError(t, err)
			require.Equal(t, base.Chunk0{
				ChunkStreamID: 2,
				Typ:           5,
				BodyLen:       4,
				Body:          []byte{0x00, 38, 37, 160},
			}, c0)

			// S->C set peer bandwidth
			err = c0.Read(conn, 128)
			require.NoError(t, err)
			require.Equal(t, base.Chunk0{
				ChunkStreamID: 2,
				Typ:           6,
				BodyLen:       5,
				Body:          []byte{0x00, 0x26, 0x25, 0xa0, 0x02},
			}, c0)

			// S->C set chunk size
			err = c0.Read(conn, 128)
			require.NoError(t, err)
			require.Equal(t, base.Chunk0{
				ChunkStreamID: 2,
				Typ:           1,
				BodyLen:       4,
				Body:          []byte{0x00, 0x01, 0x00, 0x00},
			}, c0)

			// S->C result
			err = c0.Read(conn, 65536)
			require.NoError(t, err)
			require.Equal(t, uint8(3), c0.ChunkStreamID)
			require.Equal(t, uint8(0x14), c0.Typ)
			arr, err := flvio.ParseAMFVals(c0.Body, false)
			require.NoError(t, err)
			require.Equal(t, []interface{}{
				"_result",
				float64(1),
				flvio.AMFMap{
					{K: "fmsVer", V: "LNX 9,0,124,2"},
					{K: "capabilities", V: float64(31)},
				},
				flvio.AMFMap{
					{K: "level", V: "status"},
					{K: "code", V: "NetConnection.Connect.Success"},
					{K: "description", V: "Connection succeeded."},
					{K: "objectEncoding", V: float64(0)},
				},
			}, arr)

			// C->S set chunk size
			err = base.Chunk0{
				ChunkStreamID: 2,
				Typ:           1,
				BodyLen:       4,
				Body:          []byte{0x00, 0x01, 0x00, 0x00},
			}.Write(conn)
			require.NoError(t, err)

			// C->S releaseStream
			err = base.Chunk1{
				ChunkStreamID: 3,
				Typ:           0x14,
				Body: flvio.FillAMF0ValsMalloc([]interface{}{
					"releaseStream",
					float64(2),
					nil,
					"",
				}),
			}.Write(conn)
			require.NoError(t, err)

			// C->S FCPublish
			err = base.Chunk1{
				ChunkStreamID: 3,
				Typ:           0x14,
				Body: flvio.FillAMF0ValsMalloc([]interface{}{
					"FCPublish",
					float64(3),
					nil,
					"",
				}),
			}.Write(conn)
			require.NoError(t, err)

			// C->S createStream
			err = base.Chunk3{
				ChunkStreamID: 3,
				Body: flvio.FillAMF0ValsMalloc([]interface{}{
					"createStream",
					float64(4),
					nil,
				}),
			}.Write(conn)
			require.NoError(t, err)

			// S->C result
			err = c0.Read(conn, 65536)
			require.NoError(t, err)
			require.Equal(t, uint8(3), c0.ChunkStreamID)
			require.Equal(t, uint8(0x14), c0.Typ)
			arr, err = flvio.ParseAMFVals(c0.Body, false)
			require.NoError(t, err)
			require.Equal(t, []interface{}{
				"_result",
				float64(4),
				nil,
				float64(1),
			}, arr)

			// C->S publish
			byts = flvio.FillAMF0ValsMalloc([]interface{}{
				"publish",
				float64(5),
				nil,
				"",
				"live",
			})
			err = base.Chunk0{
				ChunkStreamID: 8,
				Typ:           0x14,
				StreamID:      1,
				BodyLen:       uint32(len(byts)),
				Body:          byts,
			}.Write(conn)
			require.NoError(t, err)

			// S->C onStatus
			err = c0.Read(conn, 65536)
			require.NoError(t, err)
			require.Equal(t, uint8(5), c0.ChunkStreamID)
			require.Equal(t, uint8(0x14), c0.Typ)
			arr, err = flvio.ParseAMFVals(c0.Body, false)
			require.NoError(t, err)
			require.Equal(t, []interface{}{
				"onStatus",
				float64(5),
				nil,
				flvio.AMFMap{
					{K: "level", V: "status"},
					{K: "code", V: "NetStream.Publish.Start"},
					{K: "description", V: "publish start"},
				},
			}, arr)

			switch ca {
			case "standard":
				// C->S metadata
				byts = flvio.FillAMF0ValsMalloc([]interface{}{
					"@setDataFrame",
					"onMetaData",
					flvio.AMFMap{
						{
							K: "videodatarate",
							V: float64(0),
						},
						{
							K: "videocodecid",
							V: float64(codecH264),
						},
						{
							K: "audiodatarate",
							V: float64(0),
						},
						{
							K: "audiocodecid",
							V: float64(codecAAC),
						},
					},
				})
				err = base.Chunk0{
					ChunkStreamID: 4,
					Typ:           0x12,
					StreamID:      1,
					BodyLen:       uint32(len(byts)),
					Body:          byts,
				}.Write(conn)
				require.NoError(t, err)

				// C->S H264 decoder config
				codec := nh264.Codec{
					SPS: map[int][]byte{
						0: sps,
					},
					PPS: map[int][]byte{
						0: pps,
					},
				}
				b := make([]byte, 128)
				var n int
				codec.ToConfig(b, &n)
				body := append([]byte{flvio.FRAME_KEY<<4 | flvio.VIDEO_H264, 0, 0, 0, 0}, b[:n]...)
				err = base.Chunk0{
					ChunkStreamID: 6,
					Typ:           flvio.TAG_VIDEO,
					StreamID:      1,
					BodyLen:       uint32(len(body)),
					Body:          body,
				}.Write(conn)
				require.NoError(t, err)

				// C->S AAC decoder config
				enc, err := aac.MPEG4AudioConfig{
					Type:         2,
					SampleRate:   44100,
					ChannelCount: 2,
				}.Encode()
				require.NoError(t, err)
				err = base.Chunk0{
					ChunkStreamID: 4,
					Typ:           flvio.TAG_AUDIO,
					StreamID:      1,
					BodyLen:       uint32(len(enc) + 2),
					Body: append([]byte{
						flvio.SOUND_AAC<<4 | flvio.SOUND_44Khz<<2 | flvio.SOUND_16BIT<<1 | flvio.SOUND_STEREO,
						flvio.AAC_SEQHDR,
					}, enc...),
				}.Write(conn)
				require.NoError(t, err)

			case "metadata without codec id":
				// C->S metadata
				byts = flvio.FillAMF0ValsMalloc([]interface{}{
					"@setDataFrame",
					"onMetaData",
					flvio.AMFMap{
						{
							K: "width",
							V: float64(2688),
						},
						{
							K: "height",
							V: float64(1520),
						},
						{
							K: "framerate",
							V: float64(0o25),
						},
					},
				})
				err = base.Chunk0{
					ChunkStreamID: 4,
					Typ:           0x12,
					StreamID:      1,
					BodyLen:       uint32(len(byts)),
					Body:          byts,
				}.Write(conn)
				require.NoError(t, err)

				// C->S H264 decoder config
				codec := nh264.Codec{
					SPS: map[int][]byte{
						0: sps,
					},
					PPS: map[int][]byte{
						0: pps,
					},
				}
				b := make([]byte, 128)
				var n int
				codec.ToConfig(b, &n)
				body := append([]byte{flvio.FRAME_KEY<<4 | flvio.VIDEO_H264, 0, 0, 0, 0}, b[:n]...)
				err = base.Chunk0{
					ChunkStreamID: 6,
					Typ:           flvio.TAG_VIDEO,
					StreamID:      1,
					BodyLen:       uint32(len(body)),
					Body:          body,
				}.Write(conn)
				require.NoError(t, err)

			case "no metadata":
				// C->S H264 decoder config
				codec := nh264.Codec{
					SPS: map[int][]byte{
						0: sps,
					},
					PPS: map[int][]byte{
						0: pps,
					},
				}
				b := make([]byte, 128)
				var n int
				codec.ToConfig(b, &n)
				body := append([]byte{flvio.FRAME_KEY<<4 | flvio.VIDEO_H264, 0, 0, 0, 0}, b[:n]...)
				err = base.Chunk0{
					ChunkStreamID: 6,
					Typ:           flvio.TAG_VIDEO,
					StreamID:      1,
					BodyLen:       uint32(len(body)),
					Body:          body,
				}.Write(conn)
				require.NoError(t, err)
			}

			<-done
		})
	}
}

func TestWriteTracks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:9121")
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		require.NoError(t, err)
		defer conn.Close()

		rconn := NewServerConn(conn)
		err = rconn.ServerHandshake()
		require.NoError(t, err)

		videoTrack, err := gortsplib.NewTrackH264(96,
			[]byte{
				0x67, 0x64, 0x00, 0x0c, 0xac, 0x3b, 0x50, 0xb0,
				0x4b, 0x42, 0x00, 0x00, 0x03, 0x00, 0x02, 0x00,
				0x00, 0x03, 0x00, 0x3d, 0x08,
			},
			[]byte{
				0x68, 0xee, 0x3c, 0x80,
			},
			nil)
		require.NoError(t, err)

		audioTrack, err := gortsplib.NewTrackAAC(96, 2, 44100, 2, nil, 13, 3, 3)
		require.NoError(t, err)

		err = rconn.WriteTracks(videoTrack, audioTrack)
		require.NoError(t, err)
	}()

	conn, err := net.Dial("tcp", "127.0.0.1:9121")
	require.NoError(t, err)
	defer conn.Close()

	// C->S handshake C0
	err = base.HandshakeC0{}.Write(conn)
	require.NoError(t, err)

	// C-> handshake C1
	err = base.HandshakeC1{}.Write(conn)
	require.NoError(t, err)

	// S->C handshake S0
	err = base.HandshakeS0{}.Read(conn)
	require.NoError(t, err)

	// S->C handshake S1+S2
	s1s2 := make([]byte, 1536*2)
	_, err = conn.Read(s1s2)
	require.NoError(t, err)

	// C->S handshake C2
	err = base.HandshakeC2{}.Write(conn, s1s2)
	require.NoError(t, err)

	// C->S connect
	byts := flvio.FillAMF0ValsMalloc([]interface{}{
		"connect",
		1,
		flvio.AMFMap{
			{K: "app", V: "/stream"},
			{K: "flashVer", V: "LNX 9,0,124,2"},
			{K: "tcUrl", V: getTcURL("rtmp://127.0.0.1:9121/stream")},
			{K: "fpad", V: false},
			{K: "capabilities", V: 15},
			{K: "audioCodecs", V: 4071},
			{K: "videoCodecs", V: 252},
			{K: "videoFunction", V: 1},
		},
	})
	err = base.Chunk0{
		ChunkStreamID: 3,
		Typ:           0x14,
		BodyLen:       uint32(len(byts)),
		Body:          byts[:128],
	}.Write(conn)
	require.NoError(t, err)
	err = base.Chunk3{
		ChunkStreamID: 3,
		Body:          byts[128:],
	}.Write(conn)
	require.NoError(t, err)

	// S->C window acknowledgement size
	var c0 base.Chunk0
	err = c0.Read(conn, 128)
	require.NoError(t, err)
	require.Equal(t, base.Chunk0{
		ChunkStreamID: 2,
		Typ:           5,
		BodyLen:       4,
		Body:          []byte{0x00, 38, 37, 160},
	}, c0)

	// S->C set peer bandwidth
	err = c0.Read(conn, 128)
	require.NoError(t, err)
	require.Equal(t, base.Chunk0{
		ChunkStreamID: 2,
		Typ:           6,
		BodyLen:       5,
		Body:          []byte{0x00, 0x26, 0x25, 0xa0, 0x02},
	}, c0)

	// S->C set chunk size
	err = c0.Read(conn, 128)
	require.NoError(t, err)
	require.Equal(t, base.Chunk0{
		ChunkStreamID: 2,
		Typ:           1,
		BodyLen:       4,
		Body:          []byte{0x00, 0x01, 0x00, 0x00},
	}, c0)

	// S->C result
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(3), c0.ChunkStreamID)
	require.Equal(t, uint8(0x14), c0.Typ)
	arr, err := flvio.ParseAMFVals(c0.Body, false)
	require.NoError(t, err)
	require.Equal(t, []interface{}{
		"_result",
		float64(1),
		flvio.AMFMap{
			{K: "fmsVer", V: "LNX 9,0,124,2"},
			{K: "capabilities", V: float64(31)},
		},
		flvio.AMFMap{
			{K: "level", V: "status"},
			{K: "code", V: "NetConnection.Connect.Success"},
			{K: "description", V: "Connection succeeded."},
			{K: "objectEncoding", V: float64(0)},
		},
	}, arr)

	// C->S window acknowledgement size
	err = base.Chunk0{
		ChunkStreamID: 2,
		Typ:           0x05,
		BodyLen:       4,
		Body:          []byte{0x00, 0x26, 0x25, 0xa0},
	}.Write(conn)
	require.NoError(t, err)

	// C->S set chunk size
	err = base.Chunk0{
		ChunkStreamID: 2,
		Typ:           1,
		BodyLen:       4,
		Body:          []byte{0x00, 0x01, 0x00, 0x00},
	}.Write(conn)
	require.NoError(t, err)

	// C->S createStream
	err = base.Chunk1{
		ChunkStreamID: 3,
		Typ:           0x14,
		Body: flvio.FillAMF0ValsMalloc([]interface{}{
			"createStream",
			float64(2),
			nil,
		}),
	}.Write(conn)
	require.NoError(t, err)

	// S->C result
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(3), c0.ChunkStreamID)
	require.Equal(t, uint8(0x14), c0.Typ)
	arr, err = flvio.ParseAMFVals(c0.Body, false)
	require.NoError(t, err)
	require.Equal(t, []interface{}{
		"_result",
		float64(2),
		nil,
		float64(1),
	}, arr)

	// C->S getStreamLength
	byts = flvio.FillAMF0ValsMalloc([]interface{}{
		"getStreamLength",
		float64(3),
		nil,
		"",
	})
	err = base.Chunk0{
		ChunkStreamID: 8,
		BodyLen:       uint32(len(byts)),
		Body:          byts,
	}.Write(conn)
	require.NoError(t, err)

	// C->S play
	byts = flvio.FillAMF0ValsMalloc([]interface{}{
		"play",
		float64(4),
		nil,
		"",
		float64(-2000),
	})
	err = base.Chunk0{
		ChunkStreamID: 8,
		Typ:           0x14,
		BodyLen:       uint32(len(byts)),
		Body:          byts,
	}.Write(conn)
	require.NoError(t, err)

	// S->C event "stream is recorded"
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, base.Chunk0{
		ChunkStreamID: 2,
		Typ:           4,
		BodyLen:       6,
		Body:          []byte{0x00, 0x04, 0x00, 0x00, 0x00, 0x01},
	}, c0)

	// S->C event "stream begin 1"
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, base.Chunk0{
		ChunkStreamID: 2,
		Typ:           4,
		BodyLen:       6,
		Body:          []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
	}, c0)

	// S->C onStatus
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(5), c0.ChunkStreamID)
	require.Equal(t, uint8(0x14), c0.Typ)
	arr, err = flvio.ParseAMFVals(c0.Body, false)
	require.NoError(t, err)
	require.Equal(t, []interface{}{
		"onStatus",
		float64(4),
		nil,
		flvio.AMFMap{
			{K: "level", V: "status"},
			{K: "code", V: "NetStream.Play.Reset"},
			{K: "description", V: "play reset"},
		},
	}, arr)

	// S->C onStatus
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(5), c0.ChunkStreamID)
	require.Equal(t, uint8(0x14), c0.Typ)
	arr, err = flvio.ParseAMFVals(c0.Body, false)
	require.NoError(t, err)
	require.Equal(t, []interface{}{
		"onStatus",
		float64(4),
		nil,
		flvio.AMFMap{
			{K: "level", V: "status"},
			{K: "code", V: "NetStream.Play.Start"},
			{K: "description", V: "play start"},
		},
	}, arr)

	// S->C onStatus
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(5), c0.ChunkStreamID)
	require.Equal(t, uint8(0x14), c0.Typ)
	arr, err = flvio.ParseAMFVals(c0.Body, false)
	require.NoError(t, err)
	require.Equal(t, []interface{}{
		"onStatus",
		float64(4),
		nil,
		flvio.AMFMap{
			{K: "level", V: "status"},
			{K: "code", V: "NetStream.Data.Start"},
			{K: "description", V: "data start"},
		},
	}, arr)

	// S->C onStatus
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(5), c0.ChunkStreamID)
	require.Equal(t, uint8(0x14), c0.Typ)
	arr, err = flvio.ParseAMFVals(c0.Body, false)
	require.NoError(t, err)
	require.Equal(t, []interface{}{
		"onStatus",
		float64(4),
		nil,
		flvio.AMFMap{
			{K: "level", V: "status"},
			{K: "code", V: "NetStream.Play.PublishNotify"},
			{K: "description", V: "publish notify"},
		},
	}, arr)

	// S->C onMetadata
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(4), c0.ChunkStreamID)
	require.Equal(t, uint8(0x12), c0.Typ)
	arr, err = flvio.ParseAMFVals(c0.Body, false)
	require.NoError(t, err)
	require.Equal(t, []interface{}{
		"onMetaData",
		flvio.AMFMap{
			{K: "videodatarate", V: float64(0)},
			{K: "videocodecid", V: float64(7)},
			{K: "audiodatarate", V: float64(0)},
			{K: "audiocodecid", V: float64(10)},
		},
	}, arr)

	// S->C H264 decoder config
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(6), c0.ChunkStreamID)
	require.Equal(t, uint8(0x09), c0.Typ)
	require.Equal(t, []byte{
		0x17, 0x0, 0x0, 0x0, 0x0, 0x1, 0x64, 0x0,
		0xc, 0xff, 0xe1, 0x0, 0x15, 0x67, 0x64, 0x0,
		0xc, 0xac, 0x3b, 0x50, 0xb0, 0x4b, 0x42, 0x0,
		0x0, 0x3, 0x0, 0x2, 0x0, 0x0, 0x3, 0x0,
		0x3d, 0x8, 0x1, 0x0, 0x4, 0x68, 0xee, 0x3c,
		0x80,
	}, c0.Body)

	// S->C AAC decoder config
	err = c0.Read(conn, 65536)
	require.NoError(t, err)
	require.Equal(t, uint8(4), c0.ChunkStreamID)
	require.Equal(t, uint8(0x08), c0.Typ)
	require.Equal(t, []byte{0xae, 0x0, 0x12, 0x10}, c0.Body)
}
