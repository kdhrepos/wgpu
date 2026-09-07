module gitlab.com/akita/util

require gitlab.com/akita/protocol v0.0.0-00010101000000-000000000000

require (
	github.com/aws/aws-sdk-go v1.29.15 // indirect
	github.com/fsnotify/fsnotify v1.4.9 // indirect
	github.com/go-stack/stack v1.8.0 // indirect
	github.com/jmespath/go-jmespath v0.0.0-20180206201540-c2b33e8439af // indirect
	github.com/klauspost/compress v1.9.5 // indirect
	github.com/nxadm/tail v1.4.8 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/xdg/scram v0.0.0-20180814205039-7eeb5667e42c // indirect
	github.com/xdg/stringprep v1.0.0 // indirect
	golang.org/x/sync v0.0.0-20201020160332-67f06af15bc9 // indirect
	golang.org/x/text v0.3.5 // indirect
	gopkg.in/tomb.v1 v1.0.0-20141024135613-dd632973f1e7 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)

require (
	github.com/go-sql-driver/mysql v1.5.0
	github.com/golang/mock v1.4.4
	github.com/golang/snappy v0.0.2 // indirect
	github.com/onsi/ginkgo v1.15.0
	github.com/onsi/gomega v1.10.5
	github.com/rs/xid v1.2.1
	github.com/stretchr/testify v1.6.1 // indirect
	github.com/tebeka/atexit v0.3.0
	github.com/tidwall/pretty v1.0.2 // indirect
	gitlab.com/akita/akita v1.12.1
	gitlab.com/akita/mem v1.10.1
	go.mongodb.org/mongo-driver v1.4.1
	golang.org/x/crypto v0.0.0-20200820211705-5c72a883971a // indirect
	golang.org/x/net v0.0.0-20210119194325-5f4716e94777 // indirect
	golang.org/x/sys v0.0.0-20210218155724-8ebf48af031b // indirect
	gopkg.in/yaml.v3 v3.0.0-20200615113413-eeeca48fe776 // indirect
)

// replace gitlab.com/akita/akita => ../akita

replace gitlab.com/akita/protocol => ../protocol

go 1.25.1
