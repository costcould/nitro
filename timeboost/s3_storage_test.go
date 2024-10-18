package timeboost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

type mockS3FullClient struct {
	data map[string][]byte
}

func newmockS3FullClient() *mockS3FullClient {
	return &mockS3FullClient{make(map[string][]byte)}
}

func (m *mockS3FullClient) clear() {
	m.data = make(map[string][]byte)
}

func (m *mockS3FullClient) Client() *s3.Client {
	return nil
}

func (m *mockS3FullClient) Upload(ctx context.Context, input *s3.PutObjectInput, opts ...func(*manager.Uploader)) (*manager.UploadOutput, error) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(input.Body)
	if err != nil {
		return nil, err
	}
	m.data[*input.Key] = buf.Bytes()
	return nil, nil
}

func (m *mockS3FullClient) Download(ctx context.Context, w io.WriterAt, input *s3.GetObjectInput, options ...func(*manager.Downloader)) (n int64, err error) {
	if _, ok := m.data[*input.Key]; ok {
		ret, err := w.WriteAt(m.data[*input.Key], 0)
		if err != nil {
			return 0, err
		}
		return int64(ret), nil
	}
	return 0, errors.New("key not found")
}

func TestS3StorageServiceUploadAndDownload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockClient := newmockS3FullClient()
	s3StorageService := &S3StorageService{
		client: mockClient,
		config: &S3StorageServiceConfig{MaxBatchSize: 0},
	}

	// Test upload and download of data
	testData := []byte{1, 2, 3, 4}
	require.NoError(t, s3StorageService.uploadBatch(ctx, testData, 10))
	now := time.Now()
	key := fmt.Sprintf("validated-timeboost-bids/%d/%02d/%02d/%d.csv.gzip", now.Year(), now.Month(), now.Day(), 10)
	gotData, err := s3StorageService.downloadBatch(ctx, key)
	require.NoError(t, err)
	require.Equal(t, testData, gotData)

	// Test interaction with sqlDB and upload of multiple batches
	mockClient.clear()
	db, err := NewDatabase(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, db.InsertBid(&ValidatedBid{
		ChainId:                big.NewInt(1),
		ExpressLaneController:  common.HexToAddress("0x0000000000000000000000000000000000000001"),
		AuctionContractAddress: common.HexToAddress("0x0000000000000000000000000000000000000002"),
		Bidder:                 common.HexToAddress("0x0000000000000000000000000000000000000003"),
		Round:                  1,
		Amount:                 big.NewInt(100),
		Signature:              []byte("signature1"),
	}))
	require.NoError(t, db.InsertBid(&ValidatedBid{
		ChainId:                big.NewInt(2),
		ExpressLaneController:  common.HexToAddress("0x0000000000000000000000000000000000000004"),
		AuctionContractAddress: common.HexToAddress("0x0000000000000000000000000000000000000005"),
		Bidder:                 common.HexToAddress("0x0000000000000000000000000000000000000006"),
		Round:                  2,
		Amount:                 big.NewInt(200),
		Signature:              []byte("signature2"),
	}))
	s3StorageService.sqlDB = db

	// Helper functions to verify correctness of batch uploads and
	// Check if all the uploaded bids are removed from sql DB
	verifyBatchUploadCorrectness := func(firstRound uint64, wantBatch []byte) {
		now = time.Now()
		key = fmt.Sprintf("validated-timeboost-bids/%d/%02d/%02d/%d.csv.gzip", now.Year(), now.Month(), now.Day(), firstRound)
		s3StorageService.uploadBatches(ctx)
		data, err := s3StorageService.downloadBatch(ctx, key)
		require.NoError(t, err)
		require.Equal(t, wantBatch, data)
	}
	var sqlDBbids []*SqliteDatabaseBid
	checkUploadedBidsRemoval := func(remainingRound uint64) {
		require.NoError(t, db.sqlDB.Select(&sqlDBbids, "SELECT * FROM Bids"))
		require.Equal(t, 1, len(sqlDBbids))
		require.Equal(t, remainingRound, sqlDBbids[0].Round)
	}

	// UploadBatches should upload only the first bid and only one bid (round = 2) should remain in the sql database
	verifyBatchUploadCorrectness(1, []byte(`ChainID,Bidder,ExpressLaneController,AuctionContractAddress,Round,Amount,Signature
1,0x0000000000000000000000000000000000000003,0x0000000000000000000000000000000000000001,0x0000000000000000000000000000000000000002,1,100,signature1
`))
	checkUploadedBidsRemoval(2)

	// UploadBatches should continue adding bids to the batch until round ends, even if its past MaxBatchSize
	require.NoError(t, db.InsertBid(&ValidatedBid{
		ChainId:                big.NewInt(1),
		ExpressLaneController:  common.HexToAddress("0x0000000000000000000000000000000000000007"),
		AuctionContractAddress: common.HexToAddress("0x0000000000000000000000000000000000000008"),
		Bidder:                 common.HexToAddress("0x0000000000000000000000000000000000000009"),
		Round:                  2,
		Amount:                 big.NewInt(150),
		Signature:              []byte("signature3"),
	}))
	require.NoError(t, db.InsertBid(&ValidatedBid{
		ChainId:                big.NewInt(2),
		ExpressLaneController:  common.HexToAddress("0x0000000000000000000000000000000000000001"),
		AuctionContractAddress: common.HexToAddress("0x0000000000000000000000000000000000000002"),
		Bidder:                 common.HexToAddress("0x0000000000000000000000000000000000000003"),
		Round:                  3,
		Amount:                 big.NewInt(250),
		Signature:              []byte("signature4"),
	}))
	require.NoError(t, db.InsertBid(&ValidatedBid{
		ChainId:                big.NewInt(2),
		ExpressLaneController:  common.HexToAddress("0x0000000000000000000000000000000000000004"),
		AuctionContractAddress: common.HexToAddress("0x0000000000000000000000000000000000000005"),
		Bidder:                 common.HexToAddress("0x0000000000000000000000000000000000000006"),
		Round:                  4,
		Amount:                 big.NewInt(350),
		Signature:              []byte("signature5"),
	}))
	record := []string{sqlDBbids[0].ChainId, sqlDBbids[0].Bidder, sqlDBbids[0].ExpressLaneController, sqlDBbids[0].AuctionContractAddress, fmt.Sprintf("%d", sqlDBbids[0].Round), sqlDBbids[0].Amount, sqlDBbids[0].Signature}
	s3StorageService.config.MaxBatchSize = csvRecordSize(record)

	// Round 2 bids should all be in the same batch even though the resulting batch exceeds MaxBatchSize
	verifyBatchUploadCorrectness(2, []byte(`ChainID,Bidder,ExpressLaneController,AuctionContractAddress,Round,Amount,Signature
2,0x0000000000000000000000000000000000000006,0x0000000000000000000000000000000000000004,0x0000000000000000000000000000000000000005,2,200,signature2
1,0x0000000000000000000000000000000000000009,0x0000000000000000000000000000000000000007,0x0000000000000000000000000000000000000008,2,150,signature3
`))

	// After Batching Round 2 bids we end that batch and create a new batch for Round 3 bids to adhere to MaxBatchSize rule
	verifyBatchUploadCorrectness(3, []byte(`ChainID,Bidder,ExpressLaneController,AuctionContractAddress,Round,Amount,Signature
2,0x0000000000000000000000000000000000000003,0x0000000000000000000000000000000000000001,0x0000000000000000000000000000000000000002,3,250,signature4
`))
	checkUploadedBidsRemoval(4)
}
