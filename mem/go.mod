module gitlab.com/akita/mem

require (
	github.com/google/btree v1.0.0
	github.com/rs/xid v1.6.0
	gitlab.com/akita/akita v1.12.1
	gitlab.com/akita/lmf v0.1.0
	gitlab.com/akita/util v0.7.1
	gitlab.com/akita/mgpusim v1.10.2
	gitlab.com/akita/protocol v0.1.0
)

require (
	github.com/aws/aws-sdk-go v1.37.14 // indirect
	github.com/go-sql-driver/mysql v1.5.0 // indirect
	github.com/go-stack/stack v1.8.0 // indirect
	github.com/golang/snappy v0.0.2 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/klauspost/compress v1.11.7 // indirect
	github.com/onsi/ginkgo v1.16.4 // indirect
	github.com/onsi/gomega v1.38.2 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/stretchr/testify v1.11.0 // indirect
	github.com/tebeka/atexit v0.3.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/xdg/scram v0.0.0-20180814205039-7eeb5667e42c // indirect
	github.com/xdg/stringprep v1.0.0 // indirect
	go.mongodb.org/mongo-driver v1.4.6 // indirect
	golang.org/x/crypto v0.41.0 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/text v0.28.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)

replace gitlab.com/akita/akita => ../akita

replace gitlab.com/akita/util => ../util

replace gitlab.com/akita/mgpusim => ../mgpusim

replace gitlab.com/akita/lmf => ../lmf

replace gitlab.com/akita/protocol => ../protocol

go 1.25.1
