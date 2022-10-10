package e2e

import (
	"context"
	"github.com/labstack/echo/v4"
	"github.com/mehdihadeli/store-golang-microservice-sample/pkg/constants"
	grpcServer "github.com/mehdihadeli/store-golang-microservice-sample/pkg/grpc"
	"github.com/mehdihadeli/store-golang-microservice-sample/pkg/logger/defaultLogger"
	"github.com/mehdihadeli/store-golang-microservice-sample/pkg/messaging/bus"
	webWoker "github.com/mehdihadeli/store-golang-microservice-sample/pkg/web"
	"github.com/mehdihadeli/store-golang-microservice-sample/services/orders/config"
	"github.com/mehdihadeli/store-golang-microservice-sample/services/orders/internal/orders/configurations/mappings"
	"github.com/mehdihadeli/store-golang-microservice-sample/services/orders/internal/orders/configurations/mediatr"
	"github.com/mehdihadeli/store-golang-microservice-sample/services/orders/internal/shared/configurations/infrastructure"
	"github.com/mehdihadeli/store-golang-microservice-sample/services/orders/internal/shared/configurations/orders/metrics"
	"github.com/mehdihadeli/store-golang-microservice-sample/services/orders/internal/shared/configurations/orders/rabbitmq"
	subscriptionAll "github.com/mehdihadeli/store-golang-microservice-sample/services/orders/internal/shared/configurations/orders/subscription_all"
	"github.com/mehdihadeli/store-golang-microservice-sample/services/orders/internal/shared/contracts"
	"github.com/mehdihadeli/store-golang-microservice-sample/services/orders/internal/shared/web/workers"
	"net/http/httptest"
)

type E2ETestFixture struct {
	Echo *echo.Echo
	contracts.InfrastructureConfigurations
	V1            *V1Groups
	GrpcServer    grpcServer.GrpcServer
	HttpServer    *httptest.Server
	workersRunner *webWoker.WorkersRunner
	Bus           bus.Bus
	OrdersMetrics contracts.OrdersMetrics
	Ctx           context.Context
	cancel        context.CancelFunc
	Cleanup       func()
}

type V1Groups struct {
	OrdersGroup *echo.Group
}

func NewE2ETestFixture() *E2ETestFixture {
	cfg, _ := config.InitConfig(constants.Test)

	ctx, cancel := context.WithCancel(context.Background())
	c := infrastructure.NewInfrastructureConfigurator(defaultLogger.Logger, cfg)
	infrastructures, cleanup, err := c.ConfigInfrastructures(context.Background())
	if err != nil {
		cancel()
		return nil
	}

	echo := echo.New()

	v1Group := echo.Group("/api/v1")
	ordersV1 := v1Group.Group("/orders")

	v1Groups := &V1Groups{OrdersGroup: ordersV1}

	// this should not be in integration test because of cyclic dependencies
	err = mediatr.ConfigOrdersMediator(infrastructures)
	if err != nil {
		cancel()
		return nil
	}

	err = mappings.ConfigureOrdersMappings()
	if err != nil {
		cancel()
		return nil
	}

	mq, err := rabbitmq.ConfigOrdersRabbitMQ(ctx, cfg.RabbitMQ, infrastructures)
	if err != nil {
		cancel()
		return nil
	}

	subscriptionAllWorker, err := subscriptionAll.ConfigOrdersSubscriptionAllWorker(infrastructures, mq)
	if err != nil {
		cancel()
		return nil
	}

	ordersMetrics, err := metrics.ConfigOrdersMetrics(cfg, infrastructures.Metrics())
	if err != nil {
		cancel()
		return nil
	}

	grpcServer := grpcServer.NewGrpcServer(cfg.GRPC, defaultLogger.Logger, cfg.ServiceName, infrastructures.Metrics())
	httpServer := httptest.NewServer(echo)

	workersRunner := webWoker.NewWorkersRunner([]webWoker.Worker{
		workers.NewRabbitMQWorker(infrastructures.Log(), mq), workers.NewEventStoreDBWorker(infrastructures.Log(), infrastructures.Cfg(), subscriptionAllWorker),
	})

	return &E2ETestFixture{
		Cleanup: func() {
			workersRunner.Stop(ctx)
			cancel()
			cleanup()
			grpcServer.GracefulShutdown()
			echo.Shutdown(ctx)
			httpServer.Close()
		},
		InfrastructureConfigurations: infrastructures,
		Echo:                         echo,
		V1:                           v1Groups,
		Bus:                          mq,
		OrdersMetrics:                ordersMetrics,
		GrpcServer:                   grpcServer,
		HttpServer:                   httpServer,
		workersRunner:                workersRunner,
		Ctx:                          ctx,
		cancel:                       cancel,
	}
}

func (e *E2ETestFixture) Run() {
	go func() {
		if err := e.GrpcServer.RunGrpcServer(e.Ctx, nil); err != nil {
			e.cancel()
			e.Log().Errorf("(s.RunGrpcServer) err: %v", err)
			return
		}
	}()

	workersErr := e.workersRunner.Start(e.Ctx)
	go func() {
		for {
			select {
			case _ = <-workersErr:
				e.cancel()
				return
			}
		}
	}()
}
