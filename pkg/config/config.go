package config

type AppEnvConfig struct {
	RetryDelaySeconds int    `env:"RETRY_DELAY" envDefault:"8"`
	WebhookURL        string `env:"WEBHOOK_URL,notEmpty"`
	IngressURL        string `env:"INGRESS_URL,notEmpty"`
	ClientID          string `env:"CLIENT_ID,notEmpty"`
	UserName          string `env:"USERNAME,notEmpty"`
	Password          string `env:"PASSWORD,notEmpty"`
	TokenDuration     string `env:"TOKEN_DURATION" envDefault:"365d"`
	TokenURL          string `env:"TOKEN_URL,notEmpty"`
	SchedulerURL      string `env:"SCHEDULE_URL,notEmpty"`
	KubeConfigFile    string `env:"kubeconfigfile" envDefault:"${HOME}/.kube/config" envExpand:"true"`
	ScheduleTimeout   int64  `env:"SCHEDULE_TIMEOUT" envDefault:"45"`
}

func NewAppConfig() AppEnvConfig {
	return AppEnvConfig{}
}
