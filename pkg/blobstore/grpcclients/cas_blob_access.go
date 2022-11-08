package grpcclients

import (
	"context"
	"io"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/blobstore/slicing"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"

	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
)

type casBlobAccess struct {
	byteStreamClient                bytestream.ByteStreamClient
	compressor                      remoteexecution.Compressor_Value
	contentAddressableStorageClient remoteexecution.ContentAddressableStorageClient
	capabilitiesClient              remoteexecution.CapabilitiesClient
	uuidGenerator                   util.UUIDGenerator
	readChunkSize                   int
}

// NewCASBlobAccess creates a BlobAccess handle that relays any requests
// to a GRPC service that implements the bytestream.ByteStream and
// remoteexecution.ContentAddressableStorage services. Those are the
// services that Bazel uses to access blobs stored in the Content
// Addressable Storage.
func NewCASBlobAccess(client grpc.ClientConnInterface, compressor remoteexecution.Compressor_Value, uuidGenerator util.UUIDGenerator, readChunkSize int) blobstore.BlobAccess {
	return &casBlobAccess{
		byteStreamClient:                bytestream.NewByteStreamClient(client),
		compressor:                      compressor,
		contentAddressableStorageClient: remoteexecution.NewContentAddressableStorageClient(client),
		capabilitiesClient:              remoteexecution.NewCapabilitiesClient(client),
		uuidGenerator:                   uuidGenerator,
		readChunkSize:                   readChunkSize,
	}
}

type byteStreamChunkReader struct {
	client bytestream.ByteStream_ReadClient
	cancel context.CancelFunc
}

func (r *byteStreamChunkReader) Read() ([]byte, error) {
	chunk, err := r.client.Recv()
	if err != nil {
		return nil, err
	}
	return chunk.Data, nil
}

func (r *byteStreamChunkReader) Close() {
	r.cancel()
	for {
		if _, err := r.client.Recv(); err != nil {
			break
		}
	}
}

// Adapter allowing to read data from ByteStream.Read RPC stream through io.ReadCloser.
type byteStreamReadCloser struct {
	client bytestream.ByteStream_ReadClient
	cancel context.CancelFunc
	buf    []byte
}

func (r *byteStreamReadCloser) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		chunk, err := r.client.Recv()
		if err != nil {
			return 0, err
		}
		r.buf = chunk.Data
	}

	copied := copy(p, r.buf)

	r.buf = r.buf[copied:]
	return copied, nil
}

func (r *byteStreamReadCloser) Close() error {
	r.cancel()
	for {
		if _, err := r.client.Recv(); err != nil {
			break
		}
	}
	return nil
}

// Wrapper around zstd Decoder implementing io.ReadCloser interface.
// On close releases the zstd decoder and closes the source reader.
type zstdReaderCloser struct {
	src io.ReadCloser
	dec *zstd.Decoder
}

func (r *zstdReaderCloser) Read(p []byte) (int, error) {
	return r.dec.Read(p)
}

func (r *zstdReaderCloser) Close() error {
	// TODO: Re-use decoders for better performance.
	r.dec.Close()
	return r.src.Close()
}

func (ba *casBlobAccess) Get(ctx context.Context, digest digest.Digest) buffer.Buffer {
	ctxWithCancel, cancel := context.WithCancel(ctx)
	client, err := ba.byteStreamClient.Read(ctxWithCancel, &bytestream.ReadRequest{
		ResourceName: digest.GetByteStreamReadPath(ba.compressor),
	})
	if err != nil {
		cancel()
		return buffer.NewBufferFromError(err)
	}
	if ba.compressor == remoteexecution.Compressor_IDENTITY {
		return buffer.NewCASBufferFromChunkReader(digest, &byteStreamChunkReader{
			client: client,
			cancel: cancel,
		}, buffer.BackendProvided(buffer.Irreparable(digest)))
	}
	if ba.compressor == remoteexecution.Compressor_ZSTD {
		streamReader := &byteStreamReadCloser{
			client: client,
			cancel: cancel,
		}
		zstdDecoder, err := zstd.NewReader(streamReader)
		if err != nil {
			cancel()
			return buffer.NewBufferFromError(err)
		}
		zstdReader := &zstdReaderCloser{
			src: streamReader,
			dec: zstdDecoder,
		}

		return buffer.NewCASBufferFromReader(
			digest,
			zstdReader,
			buffer.BackendProvided(buffer.Irreparable(digest)))
	}
	panic(ba.compressor)
}

func (ba *casBlobAccess) GetFromComposite(ctx context.Context, parentDigest, childDigest digest.Digest, slicer slicing.BlobSlicer) buffer.Buffer {
	b, _ := slicer.Slice(ba.Get(ctx, parentDigest), childDigest)
	return b
}

func (ba *casBlobAccess) Put(ctx context.Context, digest digest.Digest, b buffer.Buffer) error {
	r := b.ToChunkReader(0, ba.readChunkSize)
	defer r.Close()

	ctxWithCancel, cancel := context.WithCancel(ctx)
	client, err := ba.byteStreamClient.Write(ctxWithCancel)
	if err != nil {
		cancel()
		return err
	}

	resourceName := digest.GetByteStreamWritePath(uuid.Must(ba.uuidGenerator()), remoteexecution.Compressor_IDENTITY)
	writeOffset := int64(0)
	for {
		if data, err := r.Read(); err == nil {
			// Non-terminating chunk.
			if client.Send(&bytestream.WriteRequest{
				ResourceName: resourceName,
				WriteOffset:  writeOffset,
				Data:         data,
			}) != nil {
				cancel()
				_, err := client.CloseAndRecv()
				return err
			}
			writeOffset += int64(len(data))
			resourceName = ""
		} else if err == io.EOF {
			// Terminating chunk.
			if client.Send(&bytestream.WriteRequest{
				ResourceName: resourceName,
				WriteOffset:  writeOffset,
				FinishWrite:  true,
			}) != nil {
				cancel()
				_, err := client.CloseAndRecv()
				return err
			}
			_, err := client.CloseAndRecv()
			cancel()
			return err
		} else if err != nil {
			cancel()
			client.CloseAndRecv()
			return err
		}
	}
}

func (ba *casBlobAccess) FindMissing(ctx context.Context, digests digest.Set) (digest.Set, error) {
	// Partition all digests by instance name, as the
	// FindMissingBlobs() RPC can only process digests for a single
	// instance.
	perInstanceDigests := map[digest.InstanceName][]*remoteexecution.Digest{}
	for _, digest := range digests.Items() {
		instanceName := digest.GetInstanceName()
		perInstanceDigests[instanceName] = append(perInstanceDigests[instanceName], digest.GetProto())
	}

	missingDigests := digest.NewSetBuilder()
	for instanceName, blobDigests := range perInstanceDigests {
		// Call FindMissingBlobs() for each instance.
		request := remoteexecution.FindMissingBlobsRequest{
			InstanceName: instanceName.String(),
			BlobDigests:  blobDigests,
		}
		response, err := ba.contentAddressableStorageClient.FindMissingBlobs(ctx, &request)
		if err != nil {
			return digest.EmptySet, err
		}

		// Convert results back.
		for _, proto := range response.MissingBlobDigests {
			blobDigest, err := instanceName.NewDigestFromProto(proto)
			if err != nil {
				return digest.EmptySet, err
			}
			missingDigests.Add(blobDigest)
		}
	}
	return missingDigests.Build(), nil
}

func (ba *casBlobAccess) GetCapabilities(ctx context.Context, instanceName digest.InstanceName) (*remoteexecution.ServerCapabilities, error) {
	cacheCapabilities, err := getCacheCapabilities(ctx, ba.capabilitiesClient, instanceName)
	if err != nil {
		return nil, err
	}

	// Only return fields that pertain to the Content Addressable
	// Storage. Don't set 'max_batch_total_size_bytes', as we don't
	// issue batch operations. The same holds for fields related to
	// compression support.
	return &remoteexecution.ServerCapabilities{
		CacheCapabilities: &remoteexecution.CacheCapabilities{
			DigestFunctions: digest.RemoveUnsupportedDigestFunctions(cacheCapabilities.DigestFunctions),
		},
	}, nil
}
