package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"github.com/natefinch/lumberjack"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/api"
	"github.com/xssnick/ton-payment-network/tonpayments/chain"
	chainClient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/ton-payment-network/tonpayments/metrics"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	adnlTransport "github.com/xssnick/ton-payment-network/tonpayments/transport/adnl"
	"github.com/xssnick/ton-payment-network/tonpayments/transport/web"
	pWallet "github.com/xssnick/ton-payment-network/tonpayments/wallet"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	adnlAddress "github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/rldp"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"golang.org/x/crypto/ed25519"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	_ "net/http/pprof"
)

var GitCommit = "dev"

var Verbosity = flag.Int("v", 2, "verbosity")
var DaemonMode = flag.Bool("daemon", false, "daemon mode (disables command reader)")
var Webhook = flag.String("webhook", "", "HTTP webhook address")
var API = flag.String("api", "", "HTTP API listen address")
var APICredentialsLogin = flag.String("api-login", "", "HTTP API credentials login")
var APICredentialsPassword = flag.String("api-password", "", "HTTP API credentials password")
var ConfigPath = flag.String("config", "payment-network-config.json", "config path")
var ForceBlock = flag.Uint64("force-block", 0, "master block seqno to start scan from, ignored if 0, otherwise - overrides db value")
var UseBlockScanner = flag.Bool("use-block-scanner", false, "use block scanner instead of watching specific contracts")

var LogFilename = flag.String("log-filename", "payment-network.log", "log file name")
var LogMaxSize = flag.Int("log-max-size", 1024, "maximum log file size in MB before rotation")
var LogMaxBackups = flag.Int("log-max-backups", 16, "maximum number of old log files to keep")
var LogMaxAge = flag.Int("log-max-age", 365, "maximum number of days to retain old log files")
var LogCompress = flag.Bool("log-compress", false, "whether to compress rotated log files")
var LogDisableFile = flag.Bool("log-disable-file", false, "Disable logging to file")

func main() {
	flag.Parse()

	// logs rotation
	var logWriters = []io.Writer{zerolog.NewConsoleWriter()}

	if !*LogDisableFile {
		logWriters = append(logWriters, &lumberjack.Logger{
			Filename:   *LogFilename,
			MaxSize:    *LogMaxSize, // mb
			MaxBackups: *LogMaxBackups,
			MaxAge:     *LogMaxAge, // days
			Compress:   *LogCompress,
		})
	}
	multi := zerolog.MultiLevelWriter(logWriters...)

	log.SetLogger(zerolog.New(multi).With().Timestamp().Logger().Level(zerolog.InfoLevel))
	scanLog := log.GetLogger()
	if *Verbosity >= 4 {
		scanLog = scanLog.Level(zerolog.DebugLevel).With().Logger()
	}

	if *Verbosity >= 5 {
		rldp.Logger = func(v ...any) {
			log.Debug().Msg(fmt.Sprintln(v...))
		}
		dht.Logger = func(v ...any) {
			log.Debug().Msg(fmt.Sprintln(v...))
		}
	}

	if *Verbosity >= 3 {
		log.SetLogger(log.GetLogger().Level(zerolog.DebugLevel).With().Logger())
	} else if *Verbosity == 2 {
		log.SetLogger(log.GetLogger().Level(zerolog.InfoLevel).With().Logger())
	} else if *Verbosity == 1 {
		log.SetLogger(log.GetLogger().Level(zerolog.WarnLevel).With().Logger())
	} else if *Verbosity == 0 {
		log.SetLogger(log.GetLogger().Level(zerolog.ErrorLevel).With().Logger())
	} else {
		log.SetLogger(log.GetLogger().Level(zerolog.FatalLevel).With().Logger())
	}

	log.Info().Str("version", GitCommit).Msg("starting payment node...")

	/*go func() {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		log.Info().Msg("starting pprof server on :6067")
		if err := http.ListenAndServe(":6067", nil); err != nil {
			log.Fatal().Err(err).Msg("error starting pprof server")
		}
	}()*/

	adnl.Logger = func(v ...any) {}

	if *ConfigPath == "" {
		log.Fatal().Msg("-config should have value or be not presented")
		return
	}

	cfg, err := config.LoadConfig(*ConfigPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
		return
	}

	log.Info().Msg("initializing ton client...")

	client := liteclient.NewConnectionPool()

	tonCfg, err := liteclient.GetConfigFromUrl(context.Background(), cfg.NetworkConfigUrl)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to get network config")
		return
	}

	// connect to lite servers
	err = client.AddConnectionsFromConfig(context.Background(), tonCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("ton connect err")
		return
	}

	policy := ton.ProofCheckPolicyFast
	if cfg.SecureProofPolicy {
		policy = ton.ProofCheckPolicySecure
	}

	// initialize ton api lite connection wrapper
	apiClient := ton.NewAPIClient(client, policy).WithRetry(2).WithTimeout(5 * time.Second)
	if cfg.SecureProofPolicy {
		apiClient.SetTrustedBlockFromConfig(tonCfg)
	}

	_, dhtKey, err := ed25519.GenerateKey(nil)
	dhtGate := adnl.NewGateway(dhtKey)
	if err = dhtGate.StartClient(); err != nil {
		log.Fatal().Err(err).Msg("failed to init adnl gateway for dht")
		return
	}

	dhtClient, err := dht.NewClientFromConfig(dhtGate, tonCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init dht client")
		return
	}

	if cfg.MetricsListenAddr != "" {
		metrics.RegisterMetrics(cfg.MetricsNamespace)

		go func() {
			mx := http.NewServeMux()
			mx.Handle("/metrics", promhttp.Handler())

			srv := http.Server{
				Addr:    cfg.MetricsListenAddr,
				Handler: mx,
			}
			log.Info().Str("listen", cfg.MetricsListenAddr).Msg("metrics server initialized")

			if err = srv.ListenAndServe(); err != nil {
				log.Error().Err(err).Msg("failed to start metrics server")
			}
		}()
	}

	gate := adnl.NewGateway(ed25519.NewKeyFromSeed(cfg.ADNLServerKey))

	if cfg.ExternalIP != "" {
		ip := net.ParseIP(cfg.ExternalIP)
		if ip == nil {
			log.Fatal().Msg("incorrect ip format")
			return
		}

		addr, err := netip.ParseAddrPort(cfg.NodeListenAddr)
		if err != nil {
			log.Fatal().Msg("incorrect listen addr format")
			return
		}

		gate.SetAddressList([]*adnlAddress.UDP{
			{
				IP:   ip,
				Port: int32(addr.Port()),
			},
		})
		if err := gate.StartServer(cfg.NodeListenAddr); err != nil {
			log.Fatal().Err(err).Msg("failed to init adnl gateway")
			return
		}
	} else {
		if err := gate.StartClient(); err != nil {
			log.Fatal().Err(err).Msg("failed to init adnl gateway")
			return
		}
	}

	sdb, freshDb, err := leveldb.NewLevelDB(cfg.DBPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init leveldb")
		return
	}
	fdb := db.NewDB(sdb, ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey).Public().(ed25519.PublicKey))

	if freshDb {
		if err = fdb.SetMigrationVersion(context.Background(), len(db.Migrations)); err != nil {
			log.Fatal().Err(err).Msg("failed to set initial migration version")
		}
	} else {
		if err = db.RunMigrations(fdb); err != nil {
			log.Fatal().Err(err).Msg("failed to run migrations")
		}
	}

	peerKey := ed25519.NewKeyFromSeed(cfg.ADNLServerKey)
	trs := adnlTransport.NewServer(dhtClient, gate, peerKey, ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey), cfg.ExternalIP != "")
	tr := transport.NewTransport(ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey), trs, false)

	var webTr *transport.Transport
	if cfg.WebTransportListenAddr != "" {
		wtr := web.NewHTTP(chainClient.NewTON(apiClient), peerKey)
		go func() {
			if err := wtr.StartServer(cfg.WebTransportListenAddr); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal().Err(err).Msg("failed to init web transport")
			}
		}()

		webTr = transport.NewTransport(ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey), wtr, true)
		log.Info().
			Str("listen", cfg.WebTransportListenAddr).
			Str("peer_key", base64.StdEncoding.EncodeToString(peerKey.Public().(ed25519.PublicKey))).
			Msg("web transport initialized")
	}

	var seqno uint32
	if bo, err := fdb.GetBlockOffset(context.Background()); err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			log.Fatal().Err(err).Msg("failed to load block offset")
			return
		}
	} else {
		seqno = bo.Seqno
	}

	if *ForceBlock > 0 {
		if *ForceBlock > math.MaxUint32 {
			log.Fatal().Err(err).Msg("block should be uint32")
		}
		seqno = uint32(*ForceBlock)
	}

	inv := make(chan any)
	sc := chain.NewScanner(apiClient, seqno, scanLog)

	if *UseBlockScanner {
		if err = sc.Start(context.Background(), inv); err != nil {
			log.Fatal().Err(err).Msg("failed to start block scanner")
			return
		}
	} else {
		if err = sc.StartSmall(inv); err != nil {
			log.Fatal().Err(err).Msg("failed to start account scanner")
			return
		}
		fdb.SetOnChannelUpdated(sc.OnChannelUpdate)

		chList, err := fdb.GetChannels(context.Background(), nil, db.ChannelStateAny)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to load channels")
			return
		}

		for _, channel := range chList {
			if channel.Status != db.ChannelStateInactive {
				sc.OnChannelUpdate(context.Background(), channel, true)
			}
		}

		if len(chList) > 16 {
			log.Warn().Msg("too many channels, it is recommended to switch to block scanner instead of individual account scanner by using --use-block-scanner flag")
		}
	}

	w, err := pWallet.InitWallet(apiClient, ed25519.NewKeyFromSeed(cfg.WalletPrivateKey))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init wallet")
		return
	}
	log.Info().Str("addr", w.WalletAddress().String()).Msg("wallet initialized")

	svc, err := tonpayments.NewService(chainClient.NewTON(apiClient), fdb, tr, webTr, w, inv, ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey), cfg.ChannelConfig, metrics.Registered)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init service")
		return
	}

	tr.SetService(svc)
	if webTr != nil {
		webTr.SetService(svc)
	}

	log.Info().Str("pubkey", base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey).Public().(ed25519.PublicKey))).Msg("payment node initialized")

	if !*DaemonMode {
		go func() {
			for {
				if err := commandReader(svc, cfg, fdb, w.Wallet(), apiClient); err != nil {
					log.Error().Err(err).Msg("command failed")
				}
			}
		}()
	}

	if *API != "" {
		var credentials *api.Credentials
		if *APICredentialsLogin != "" || *APICredentialsPassword != "" {
			if *APICredentialsLogin == "" || *APICredentialsPassword == "" {
				log.Fatal().Msg("both api login and password must be set in the same time")
				return
			}

			credentials = &api.Credentials{
				Login:    *APICredentialsLogin,
				Password: *APICredentialsPassword,
			}
		}

		srv := api.NewServer(*API, *Webhook, cfg.WebhooksSignatureHMACSHA256Key, svc, fdb, credentials)
		if *Webhook != "" {
			svc.SetWebhook(srv)
		}

		go func() {
			if err := srv.Start(); err != nil {
				log.Error().Err(err).Msg("failed to start api server")
			}
		}()

		log.Info().Str("api", *API).Str("webhook", *Webhook).Msg("api initialized")
	}

	svc.Start()
}

func commandReader(svc *tonpayments.Service, cfg *config.Config, fdb *db.DB, wlt *wallet.Wallet, apiClient ton.APIClientWrapped) error {
	var cmd string
	_, _ = fmt.Scanln(&cmd)

	switch cmd {
	case "list":
		svc.DebugPrintVirtualChannels()
	case "inc":
		log.Info().Msg("input channel address to run increment state test:")
		var addr string
		_, _ = fmt.Scanln(&addr)

		if err := svc.IncrementStates(context.Background(), addr, true); err != nil {
			return fmt.Errorf("failed to increment states with channel: %w", err)
		}
		log.Info().Msg("increment task created")
	case "inc-hard":
		log.Info().Msg("input channel address to run increment state test:")
		var addr string
		_, _ = fmt.Scanln(&addr)

		for i := 0; i < 3000; i++ {
			if err := svc.IncrementStates(context.Background(), addr, true); err != nil {
				return fmt.Errorf("failed to increment states with channel: %w", err)
			}
		}
		log.Info().Msg("tasks created")
	case "destroy":
		log.Info().Msg("to start cooperative close input channel address:")
		var addr string
		_, _ = fmt.Scanln(&addr)

		if err := svc.RequestCooperativeClose(context.Background(), addr); err != nil {
			return fmt.Errorf("failed to close channel cooperatively: %w", err)
		}
		log.Info().Msg("cooperative channel closure attempt has been started")
	case "kill":
		log.Info().Msg("to start uncooperative close input channel address:")
		var addr string
		_, _ = fmt.Scanln(&addr)

		if err := svc.RequestUncooperativeClose(context.Background(), addr); err != nil {
			return fmt.Errorf("failed to close channel uncooperatively: %w", err)
		}
		log.Info().Msg("uncooperative channel closure has been started")
	case "sign":
		log.Info().Msg("input virtual channel private key:")
		var strKey string
		_, _ = fmt.Scanln(&strKey)

		btsKey, err := base64.StdEncoding.DecodeString(strKey)
		if err != nil {
			return fmt.Errorf("incorrect format of key: %w", err)
		}
		if len(btsKey) != 32 {
			return fmt.Errorf("incorrect len of key: %d, should be 32", len(btsKey))
		}

		vcKey := ed25519.NewKeyFromSeed(btsKey)

		meta, err := svc.GetVirtualChannelMeta(context.Background(), vcKey.Public().(ed25519.PublicKey))
		if err != nil {
			return fmt.Errorf("failed to get virtual channel meta: %w", err)
		}

		if meta.FinalDestination == nil {
			return fmt.Errorf("you are not initiator of this virtual channel")
		}

		ch, err := svc.GetChannel(context.Background(), meta.Outgoing.ChannelAddress)
		if err != nil {
			return fmt.Errorf("failed to get channel: %w", err)
		}

		cc, err := svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, false)
		if err != nil {
			return fmt.Errorf("failed to get coin config: %w", err)
		}

		log.Info().Msg("input amount:")
		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromDecimal(strAmt, int(cc.Decimals))
		if err != nil {
			return fmt.Errorf("incorrect format of amount")
		}

		state, enc, err := payments.SignState(amt, vcKey, meta.FinalDestination)
		if err != nil {
			return fmt.Errorf("failed to sign state: %w", err)
		}

		if err = svc.AddVirtualChannelResolve(context.Background(), vcKey.Public().(ed25519.PublicKey), state); err != nil {
			return fmt.Errorf("failed to add resolve to channel: %w", err)
		}

		log.Info().Str("signed_state", base64.StdEncoding.EncodeToString(enc)).Msg("state was signed")
	case "close":
		log.Info().Msg("enter the virtual channel final state base64:")

		var stateStr string
		_, _ = fmt.Scanln(&stateStr)

		btsState, err := base64.StdEncoding.DecodeString(stateStr)
		if err != nil {
			return fmt.Errorf("incorrect format of state: %w", err)
		}

		key, state, err := payments.ParseState(btsState, svc.GetPrivateKey())
		if err != nil {
			return fmt.Errorf("incorrect state: %w", err)
		}

		err = svc.AddVirtualChannelResolve(context.Background(), key, state)
		if err != nil {
			return fmt.Errorf("failed to add resolve to channel: %w", err)
		}

		err = svc.CloseVirtualChannel(context.Background(), key)
		if err != nil {
			return fmt.Errorf("failed to close channel: %w", err)
		}
		log.Info().Msg("virtual channel closure requested")
	case "ask-remove":
		log.Info().Msg("input virtual channel public key:")
		var strKey string
		_, _ = fmt.Scanln(&strKey)

		btsKey, err := base64.StdEncoding.DecodeString(strKey)
		if err != nil {
			return fmt.Errorf("incorrect format of key: %w", err)
		}
		if len(btsKey) != 32 {
			return fmt.Errorf("incorrect len of key: %d, should be 32", len(btsKey))
		}

		if err = svc.RequestRemoveVirtual(context.Background(), btsKey); err != nil {
			return fmt.Errorf("failed to remove virtual channel: %w", err)
		}
	case "topup":
		log.Info().Msg("enter channel address to topup:")

		var addrStr string
		_, _ = fmt.Scanln(&addrStr)

		addr, err := address.ParseAddr(addrStr)
		if err != nil {
			return fmt.Errorf("incorrect format of address: %w", err)
		}

		ch, err := svc.GetChannel(context.Background(), addrStr)
		if err != nil {
			return fmt.Errorf("failed to get channel: %w", err)
		}

		cc, err := svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, true)
		if err != nil {
			return fmt.Errorf("failed to get coin config: %w", err)
		}

		log.Info().Msg("input amount:")
		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromDecimal(strAmt, int(cc.Decimals))
		if err != nil {
			return fmt.Errorf("incorrect format of amount")
		}

		if err = svc.TopupChannel(context.Background(), addr, amt); err != nil {
			return fmt.Errorf("failed to topup channel: %w", err)
		}
	case "withdraw":
		log.Info().Msg("enter channel address to withdraw from:")

		var addrStr string
		_, _ = fmt.Scanln(&addrStr)

		addr, err := address.ParseAddr(addrStr)
		if err != nil {
			return fmt.Errorf("incorrect format of address: %w", err)
		}

		ch, err := svc.GetChannel(context.Background(), addrStr)
		if err != nil {
			return fmt.Errorf("failed to get channel: %w", err)
		}

		cc, err := svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, true)
		if err != nil {
			return fmt.Errorf("failed to get coin config: %w", err)
		}

		log.Info().Msg("input amount:")
		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromDecimal(strAmt, int(cc.Decimals))
		if err != nil {
			return fmt.Errorf("incorrect format of amount")
		}

		if err = svc.RequestWithdraw(context.Background(), addr, amt); err != nil {
			return fmt.Errorf("failed to withdraw from channel: %w", err)
		}
	case "deploy":
		log.Info().Msg("enter the key of node to deploy channel with:")

		var strKey string
		_, _ = fmt.Scanln(&strKey)

		btsKey, err := base64.StdEncoding.DecodeString(strKey)
		if err != nil {
			return fmt.Errorf("incorrect format of key: %w", err)
		}
		if len(btsKey) != 32 {
			return fmt.Errorf("incorrect len of key: %d, should be 32", len(btsKey))
		}

		log.Info().Msg("input jetton master address or extra currency id, or skip for ton:")
		var jetton string
		_, _ = fmt.Scanln(&jetton)

		var ecID uint64
		var jettonMaster *address.Address
		if jetton != "" {
			ecID, err = strconv.ParseUint(jetton, 10, 32)
			if err != nil {
				jettonMaster, err = address.ParseAddr(jetton)
				if err != nil {
					return fmt.Errorf("incorrect format: %w", err)
				}
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		addr, err := svc.DeployChannelWithNode(ctx, btsKey, jettonMaster, uint32(ecID))
		cancel()
		if err != nil {
			return fmt.Errorf("failed to deploy channel with node: %w", err)
		}
		log.Info().Str("address", addr.String()).Msg("onchain channel deployed")
	case "wallet-ton-transfer":
		log.Info().Msg("enter address to transfer to:")

		var addrStr string
		_, _ = fmt.Scanln(&addrStr)

		addr, err := address.ParseAddr(addrStr)
		if err != nil {
			return fmt.Errorf("incorrect format of address: %w", err)
		}

		log.Info().Msg("input amount:")
		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromTON(strAmt)
		if err != nil {
			return fmt.Errorf("incorrect format of amount")
		}

		log.Info().Msg("input comment:")
		var comment string
		_, _ = fmt.Scanln(&comment)

		log.Info().
			Str("to_address", addr.String()).
			Str("amount", amt.String()).
			Msg("transferring...")

		time.Sleep(3 * time.Second) // give user some time to cancel

		tx, _, err := wlt.TransferWaitTransaction(context.Background(), addr, amt, comment)
		if err != nil {
			return fmt.Errorf("failed to transfer: %w", err)
		}
		log.Info().Str("hash", base64.URLEncoding.EncodeToString(tx.Hash)).Msg("transfer transaction committed")
	case "wallet-ton-balance":
		blk, err := apiClient.CurrentMasterchainInfo(context.Background())
		if err != nil {
			return fmt.Errorf("failed to get current masterchain info: %w", err)
		}

		balance, err := wlt.GetBalance(context.Background(), blk)
		if err != nil {
			return fmt.Errorf("failed to get balance: %w", err)
		}

		log.Info().Msgf("wallet balance: %s TON", balance.String())
	case "open", "send":
		log.Info().Msg("enter nodes to tunnel virtual channel through, including receiver (',' separated):")
		var strKeys string
		_, _ = fmt.Scanln(&strKeys)

		keys := strings.Split(strings.ReplaceAll(strKeys, " ", ""), ",")

		var err error
		var parsedKeys [][]byte
		for _, strKey := range keys {
			btsKey, err := base64.StdEncoding.DecodeString(strKey)
			if err != nil {
				return fmt.Errorf("incorrect format of key: %w", err)
			}
			if len(btsKey) != 32 {
				return fmt.Errorf("incorrect len of key: %d, should be 32", len(btsKey))
			}

			parsedKeys = append(parsedKeys, btsKey)
		}

		log.Info().Msg("input jetton master address or extra currency id, or skip for ton:")
		var jetton string
		_, _ = fmt.Scanln(&jetton)

		var ecID uint64
		var jettonMaster *address.Address
		var jettonMasterStr string
		if jetton != "" {
			ecID, err = strconv.ParseUint(jetton, 10, 32)
			if err != nil {
				jettonMaster, err = address.ParseAddr(jetton)
				if err != nil {
					return fmt.Errorf("incorrect format: %w", err)
				}
				jettonMasterStr = jettonMaster.Bounce(true).String()
			}
		}

		log.Info().Msg("input amount, excluding tunnelling fee:")

		cc, err := svc.ResolveCoinConfig(jettonMasterStr, uint32(ecID), false)
		if err != nil {
			return fmt.Errorf("failed to get coin config: %w", err)
		}

		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromDecimal(strAmt, int(cc.Decimals))
		if err != nil {
			return fmt.Errorf("incorrect format of amount")
		}

		log.Info().Msg("input fee amount per each proxy node:")

		var strAmtFee string
		_, _ = fmt.Scanln(&strAmtFee)
		if strAmtFee == "" {
			strAmtFee = "0"
		}

		feeAmt, err := tlb.FromDecimal(strAmtFee, int(cc.Decimals))
		if err != nil {
			return fmt.Errorf("incorrect format of fee amount")
		}

		safeHopTTL := time.Duration(cfg.ChannelConfig.QuarantineDurationSec+cfg.ChannelConfig.BufferTimeToCommit+cfg.ChannelConfig.ConditionalCloseDurationSec+
			cfg.ChannelConfig.MinSafeVirtualChannelTimeoutSec) * time.Second

		fullAmt := new(big.Int).Set(amt.Nano())
		var tunChain []transport.TunnelChainPart
		for i, parsedKey := range parsedKeys {
			fee := big.NewInt(0)
			if len(parsedKeys)-i > 1 {
				fee = new(big.Int).Mul(feeAmt.Nano(), big.NewInt(int64(len(parsedKeys)-i)-1))
				fullAmt = fullAmt.Add(fullAmt, fee)
			}

			tunChain = append(tunChain, transport.TunnelChainPart{
				Target:   parsedKey,
				Capacity: amt.Nano(),
				Fee:      fee,
				Deadline: time.Now().Add(1*time.Minute + safeHopTTL*time.Duration(len(parsedKeys)-i)),
			})
		}

		_, vPriv, _ := ed25519.GenerateKey(nil)
		vc, firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 5, cmd == "send", svc.GetPrivateKey())
		if err != nil {
			return fmt.Errorf("failed to generate tunnel: %w", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err = svc.OpenVirtualChannel(ctx, tunChain[0].Target, firstInstructionKey, tunChain[len(tunChain)-1].Target, vPriv, tun, vc, jettonMaster, uint32(ecID))
		cancel()
		if err != nil {
			return fmt.Errorf("failed to open virtual channel with node: %w", err)
		}

		if cmd != "send" {
			log.Info().
				Str("private_key", base64.StdEncoding.EncodeToString(vPriv.Seed())).
				Str("total_amount", tlb.MustFromNano(fullAmt, int(cc.Decimals)).String()).
				Str("capacity", amt.String()).
				Msg("virtual channel opening requested")
		} else {
			log.Info().
				Str("total_amount", tlb.MustFromNano(fullAmt, int(cc.Decimals)).String()).
				Str("amount", amt.String()).
				Msg("virtual transfer requested")
		}
	case "virtual-commit-all":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := svc.CommitAllOurVirtualChannelsAndWait(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("failed to commit all virtual channels: %w", err)
		}
		log.Info().Msg("all virtual channels committed")
	case "debug-tasks", "debug-tasks-all":
		log.Info().Msg("input tasks prefix to search:")
		var pfx string
		_, _ = fmt.Scanln(&pfx)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		list, err := fdb.DumpTasks(ctx, pfx)
		cancel()
		if err != nil {
			log.Error().Err(err).Msg("failed to load planned tasks")
			break
		}

		for _, task := range list {
			if task.CompletedAt != nil {
				if cmd == "debug-tasks-all" {
					log.Info().Str("type", task.Type).
						Str("id", task.ID).
						Time("created_at", task.CreatedAt).
						Time("completed_at", *task.CompletedAt).
						Msg("completed task")
				}
				continue
			}

			if task.ExecuteTill != nil && task.ExecuteTill.Before(time.Now()) {
				if cmd == "debug-tasks-all" {
					log.Info().Str("type", task.Type).
						Str("id", task.ID).
						Time("created_at", task.CreatedAt).
						Time("execute_till", *task.ExecuteTill).
						Msg("outdated task")
				}
				continue
			}

			log.Info().Str("type", task.Type).
				Str("id", task.ID).
				Time("created_at", task.CreatedAt).
				Str("last_error", task.LastError).
				Time("after", task.ExecuteAfter).
				Str("queue", task.Queue).
				Msg("planned task")
		}
		log.Info().Msg("done")
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}

	return nil
}
