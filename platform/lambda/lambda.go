// Package lambda implements the API Gateway & AWS Lambda platform.
package lambda

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/apex/log"
	"github.com/apex/log/handlers/discard"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/aws/aws-sdk-go/service/apigateway"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/dchest/uniuri"
	"github.com/dustin/go-humanize"
	"github.com/golang/sync/errgroup"
	"github.com/pkg/errors"

	"github.com/apex/up"
	"github.com/apex/up/config"
	"github.com/apex/up/internal/proxy/bin"
	"github.com/apex/up/internal/shim"
	"github.com/apex/up/internal/util"
	"github.com/apex/up/internal/zip"
	"github.com/apex/up/platform"
	"github.com/apex/up/platform/event"
	"github.com/apex/up/platform/lambda/runtime"
	"github.com/apex/up/platform/lambda/stack"
)

// errFirstDeploy is returned from .deploy() when a function is created.
var errFirstDeploy = errors.New("first deploy")

const (
	// maxCodeSize is the max code size supported by Lambda (250MiB).
	maxCodeSize = 250 << 20
)

// assume policy for the lambda function.
var apiGatewayAssumePolicy = `{
	"Version": "2012-10-17",
	"Statement": [
		{
			"Effect": "Allow",
			"Principal": {
				"Service": "apigateway.amazonaws.com"
			},
			"Action": "sts:AssumeRole"
		},
		{
      "Effect": "Allow",
      "Principal": {
        "Service": "lambda.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
	]
}`

// policy for the lambda function.
var functionPolicy = `{
	"Version": "2012-10-17",
	"Statement": [
		{
			"Effect": "Allow",
			"Resource": "*",
			"Action": [
				"logs:CreateLogGroup",
				"logs:CreateLogStream",
				"logs:PutLogEvents",
				"ssm:GetParametersByPath"
			]
		}
	]
}`

// TODO: aggregate progress report for N regions or distinct progress bars
// TODO: refactor with another region-scoped struct to clean this up

// Platform implementation.
type Platform struct {
	config  *up.Config
	runtime string
	handler string
	zip     *bytes.Buffer
	events  event.Events
}

// New platform.
func New(c *up.Config, events event.Events) *Platform {
	return &Platform{
		config:  c,
		runtime: "nodejs6.10",
		handler: "_proxy.handle",
		events:  events,
	}
}

// Build implementation.
func (p *Platform) Build() error {
	start := time.Now()
	p.zip = new(bytes.Buffer)

	if err := p.injectProxy(); err != nil {
		return errors.Wrap(err, "injecting proxy")
	}
	defer p.removeProxy()

	r, stats, err := zip.Build(".")
	if err != nil {
		return errors.Wrap(err, "zip")
	}

	if _, err := io.Copy(p.zip, r); err != nil {
		return errors.Wrap(err, "copying")
	}

	if err := r.Close(); err != nil {
		return errors.Wrap(err, "closing")
	}

	p.events.Emit("platform.build.zip", event.Fields{
		"files":             stats.FilesAdded,
		"size_uncompressed": stats.SizeUncompressed,
		"size_compressed":   p.zip.Len(),
		"duration":          time.Since(start),
	})

	if stats.SizeUncompressed > maxCodeSize {
		size := humanize.Bytes(uint64(stats.SizeUncompressed))
		max := humanize.Bytes(uint64(maxCodeSize))
		return errors.Errorf("zip contents is %s, exceeding Lambda's limit of %s", size, max)
	}

	return nil
}

// Zip returns the zip reader.
func (p *Platform) Zip() io.Reader {
	return p.zip
}

// Init initializes the runtime.
func (p *Platform) Init(stage string) error {
	return runtime.New(
		p.config,
		runtime.WithLogger(&log.Logger{
			Handler: discard.Default,
		}),
	).Init(stage)
}

// Deploy implementation.
func (p *Platform) Deploy(stage string) error {
	regions := p.config.Regions
	var g errgroup.Group

	if err := p.createRole(); err != nil {
		return errors.Wrap(err, "iam")
	}

	for _, r := range regions {
		region := r
		g.Go(func() error {
			version, err := p.deploy(region, stage)
			if err == nil {
				return nil
			}

			if err != errFirstDeploy {
				return errors.Wrap(err, region)
			}

			if err := p.CreateStack(region, version); err != nil {
				return errors.Wrap(err, region)
			}

			return nil
		})
	}

	return g.Wait()
}

// Logs implementation.
func (p *Platform) Logs(region, query string) platform.Logs {
	return NewLogs(p, region, query)
}

// Domains implementation.
func (p *Platform) Domains() platform.Domains {
	return NewDomains()
}

// URL returns the stage url.
func (p *Platform) URL(region, stage string) (string, error) {
	s := session.New(aws.NewConfig().WithRegion(region))
	c := apigateway.New(s)

	api, err := p.getAPI(c)
	if err != nil {
		return "", errors.Wrap(err, "fetching api")
	}

	if api == nil {
		return "", errors.Errorf("cannot find the API, looks like you haven't deployed")
	}

	id := fmt.Sprintf("https://%s.execute-api.%s.amazonaws.com/%s/", *api.Id, region, stage)
	return id, nil
}

// CreateStack implementation.
func (p *Platform) CreateStack(region, version string) error {
	if err := p.createCerts(); err != nil {
		return errors.Wrap(err, "creating certs")
	}

	zones, err := p.getHostedZone()
	if err != nil {
		return errors.Wrap(err, "fetching zones")
	}

	return stack.New(p.config, p.events, zones, region).Create(version)
}

// DeleteStack implementation.
func (p *Platform) DeleteStack(region string, wait bool) error {
	if err := p.createRole(); err != nil {
		return errors.Wrap(err, "creating iam role")
	}

	if err := p.deleteBucket(region); err != nil && !util.IsNotFound(err) {
		return errors.Wrap(err, "deleting s3 bucket")
	}

	log.Debug("deleting stack")
	if err := stack.New(p.config, p.events, nil, region).Delete(wait); err != nil {
		return errors.Wrap(err, "deleting stack")
	}

	log.Debug("deleting function")
	if err := p.deleteFunction(region); err != nil && !util.IsNotFound(err) {
		return errors.Wrap(err, "deleting function")
	}

	log.Debug("deleting role")
	if err := p.deleteRole(region); err != nil && !util.IsNotFound(err) {
		return errors.Wrap(err, "deleting role")
	}

	return nil
}

// ShowStack implementation.
func (p *Platform) ShowStack(region string) error {
	return stack.New(p.config, p.events, nil, region).Show()
}

// PlanStack implementation.
func (p *Platform) PlanStack(region string) error {
	if err := p.createCerts(); err != nil {
		return errors.Wrap(err, "creating certs")
	}

	zones, err := p.getHostedZone()
	if err != nil {
		return errors.Wrap(err, "fetching zones")
	}

	return stack.New(p.config, p.events, zones, region).Plan()
}

// ApplyStack implementation.
func (p *Platform) ApplyStack(region string) error {
	if err := p.createCerts(); err != nil {
		return errors.Wrap(err, "creating certs")
	}

	return stack.New(p.config, p.events, nil, region).Apply()
}

// getHostedZone returns existing hosted zones.
func (p *Platform) getHostedZone() (zones []*route53.HostedZone, err error) {
	r := route53.New(session.New(aws.NewConfig()))

	log.Debug("fetching hosted zones")
	res, err := r.ListHostedZonesByName(&route53.ListHostedZonesByNameInput{
		MaxItems: aws.String("100"),
	})

	if err != nil {
		return
	}

	zones = res.HostedZones
	return
}

// createCerts creates the certificates if necessary.
//
// We perform this task outside of CloudFormation because
// the certificates currently must be created in the us-east-1
// region. This also gives us a chance to let the user know
// that they have to confirm an email.
func (p *Platform) createCerts() error {
	s := session.New(aws.NewConfig().WithRegion("us-east-1"))
	a := acm.New(s)

	// existing certs
	res, err := a.ListCertificates(&acm.ListCertificatesInput{
		MaxItems: aws.Int64(1000),
	})

	if err != nil {
		return errors.Wrap(err, "listing")
	}

	var domains []string

	// request certs
	for _, s := range p.config.Stages.List() {
		if s == nil {
			continue
		}

		// see if the cert exists
		log.Debugf("looking up cert for %s", s.Domain)
		arn := getCert(res.CertificateSummaryList, s.Domain)
		if arn != "" {
			log.Debugf("found cert for %s: %s", s.Domain, arn)
			s.Cert = arn
			continue
		}

		option := acm.DomainValidationOption{
			DomainName:       &s.Domain,
			ValidationDomain: aws.String(util.Domain(s.Domain)),
		}

		options := []*acm.DomainValidationOption{
			&option,
		}

		// request the cert
		res, err := a.RequestCertificate(&acm.RequestCertificateInput{
			DomainName:              &s.Domain,
			DomainValidationOptions: options,
		})

		if err != nil {
			return errors.Wrapf(err, "requesting cert for %s", s.Domain)
		}

		domains = append(domains, s.Domain)
		s.Cert = *res.CertificateArn
	}

	// no certs needed
	if len(domains) == 0 {
		return nil
	}

	defer p.events.Time("platform.certs.create", event.Fields{
		"domains": domains,
	})()

	// wait for approval
	for range time.Tick(4 * time.Second) {
		res, err = a.ListCertificates(&acm.ListCertificatesInput{
			MaxItems:            aws.Int64(1000),
			CertificateStatuses: aws.StringSlice([]string{acm.CertificateStatusPendingValidation}),
		})

		if err != nil {
			return errors.Wrap(err, "listing")
		}

		if len(res.CertificateSummaryList) == 0 {
			break
		}
	}

	return nil
}

// deploy to the given region.
func (p *Platform) deploy(region, stage string) (version string, err error) {
	start := time.Now()

	fields := event.Fields{
		"stage":  stage,
		"region": region,
	}

	p.events.Emit("platform.deploy", fields)

	defer func() {
		fields["duration"] = time.Since(start)
		fields["version"] = version
		p.events.Emit("platform.deploy.complete", fields)
	}()

	ctx := log.WithField("region", region)
	s := session.New(aws.NewConfig().WithRegion(region))
	u := s3manager.NewUploaderWithClient(s3.New(s))
	a := apigateway.New(s)
	c := lambda.New(s)

	ctx.Debug("fetching function config")
	_, err = c.GetFunctionConfiguration(&lambda.GetFunctionConfigurationInput{
		FunctionName: &p.config.Name,
	})

	if util.IsNotFound(err) {
		defer p.events.Time("platform.function.create", fields)
		return p.createFunction(c, a, u, region, stage)
	}

	if err != nil {
		return "", errors.Wrap(err, "fetching function config")
	}

	defer p.events.Time("platform.function.update", fields)
	return p.updateFunction(c, a, u, region, stage)
}

// createFunction creates the function.
func (p *Platform) createFunction(c *lambda.Lambda, a *apigateway.APIGateway, up *s3manager.Uploader, region, stage string) (version string, err error) {
	if err := p.createBucket(region); err != nil {
		return "", errors.Wrap(err, "creating s3 bucket")
	}

	log.Debug("uploading function")
retry:
	b := aws.String(p.getS3BucketName())
	k := aws.String(p.getS3Key(stage))

	_, err = up.Upload(&s3manager.UploadInput{
		Bucket: b,
		Key:    k,
		Body:   bytes.NewReader(p.zip.Bytes()),
	})

	if err != nil {
		return "", errors.Wrap(err, "uploading function")
	}

	log.Debug("creating function")
	res, err := c.CreateFunction(&lambda.CreateFunctionInput{
		FunctionName: &p.config.Name,
		Handler:      &p.handler,
		Runtime:      &p.runtime,
		Role:         &p.config.Lambda.Role,
		MemorySize:   aws.Int64(int64(p.config.Lambda.Memory)),
		Timeout:      aws.Int64(int64(p.config.Proxy.Timeout + 3)),
		Publish:      aws.Bool(true),
		Environment:  toEnv(p.config.Environment, stage),
		Code: &lambda.FunctionCode{
			S3Bucket: b,
			S3Key:    k,
		},
	})

	// IAM is eventually consistent apparently, so we have to keep retrying
	if isCreatingRole(err) {
		log.Debug("waiting for role to be created")
		time.Sleep(500 * time.Millisecond)
		goto retry
	}

	if err != nil {
		return "", errors.Wrap(err, "creating function")
	}

	return *res.Version, errFirstDeploy
}

// updateFunction updates the function.
func (p *Platform) updateFunction(c *lambda.Lambda, a *apigateway.APIGateway, up *s3manager.Uploader, region, stage string) (version string, err error) {
	var publish bool

	if stage != "development" {
		publish = true
		log.Debug("publishing new version")
	}

	b := aws.String(p.getS3BucketName())
	k := aws.String(p.getS3Key(stage))

retry:
	log.Debug("uploading function")
	_, err = up.Upload(&s3manager.UploadInput{
		Bucket: b,
		Key:    k,
		Body:   bytes.NewReader(p.zip.Bytes()),
	})

	if util.IsNotFound(err) {
		if err := p.createBucket(region); err != nil {
			return "", errors.Wrap(err, "creating s3 bucket")
		}
		goto retry
	}

	if err != nil {
		return "", errors.Wrap(err, "uploading function")
	}

	log.Debug("updating function")
	_, err = c.UpdateFunctionConfiguration(&lambda.UpdateFunctionConfigurationInput{
		FunctionName: &p.config.Name,
		Handler:      &p.handler,
		Runtime:      &p.runtime,
		Role:         &p.config.Lambda.Role,
		MemorySize:   aws.Int64(int64(p.config.Lambda.Memory)),
		Timeout:      aws.Int64(int64(p.config.Proxy.Timeout + 3)),
		Environment:  toEnv(p.config.Environment, stage),
	})

	if err != nil {
		return "", errors.Wrap(err, "updating function config")
	}

	log.Debug("updating function code")
	res, err := c.UpdateFunctionCode(&lambda.UpdateFunctionCodeInput{
		FunctionName: &p.config.Name,
		Publish:      &publish,
		S3Bucket:     b,
		S3Key:        k,
	})

	if err != nil {
		return "", errors.Wrap(err, "updating function code")
	}

	if publish {
		log.Debugf("alias %s to %s", stage, *res.Version)
		_, err := c.UpdateAlias(&lambda.UpdateAliasInput{
			FunctionName:    &p.config.Name,
			FunctionVersion: res.Version,
			Name:            &stage,
		})

		if err != nil {
			return "", errors.Wrap(err, "creating function alias")
		}

		return *res.Version, nil
	}

	return "", nil
}

// deleteFunction deletes the lambda function.
func (p *Platform) deleteFunction(region string) error {
	// TODO: sessions all over... refactor
	c := lambda.New(session.New(aws.NewConfig().WithRegion(region)))

	_, err := c.DeleteFunction(&lambda.DeleteFunctionInput{
		FunctionName: &p.config.Name,
	})

	return err
}

// createRole creates the IAM role unless it is present.
func (p *Platform) createRole() error {
	s := session.New(aws.NewConfig())
	c := iam.New(s)

	name := fmt.Sprintf("%s-function", p.config.Name)
	desc := util.ManagedByUp("")

	if s := p.config.Lambda.Role; s != "" {
		log.Debugf("using role from config %s", s)
		return nil
	}

	log.Debug("checking for role")
	existing, err := c.GetRole(&iam.GetRoleInput{
		RoleName: &name,
	})

	// network or permission error
	if err != nil && !util.IsNotFound(err) {
		return errors.Wrap(err, "fetching role")
	}

	// use the existing role
	if err == nil {
		arn := *existing.Role.Arn
		log.Debugf("using existing role %s", arn)
		p.config.Lambda.Role = arn
		return nil
	}

	log.Debug("creating role")
	role, err := c.CreateRole(&iam.CreateRoleInput{
		RoleName:                 &name,
		Description:              &desc,
		AssumeRolePolicyDocument: &apiGatewayAssumePolicy,
	})

	if err != nil {
		return errors.Wrap(err, "creating role")
	}

	log.Debug("attaching policy")
	_, err = c.PutRolePolicy(&iam.PutRolePolicyInput{
		PolicyName:     &name,
		RoleName:       &name,
		PolicyDocument: &functionPolicy,
	})

	if err != nil {
		return errors.Wrap(err, "attaching policy")
	}

	arn := *role.Role.Arn
	log.Debugf("set role to %s", arn)
	p.config.Lambda.Role = arn

	return nil
}

// deleteRole deletes the role and policy.
func (p *Platform) deleteRole(region string) error {
	name := fmt.Sprintf("%s-function", p.config.Name)
	c := iam.New(session.New(aws.NewConfig().WithRegion(region)))

	_, err := c.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
		RoleName:   &name,
		PolicyName: &name,
	})

	if err != nil {
		return errors.Wrap(err, "deleting role policy")
	}

	_, err = c.DeleteRole(&iam.DeleteRoleInput{
		RoleName: &name,
	})

	if err != nil {
		return errors.Wrap(err, "deleting role")
	}

	return nil
}

// createBucket creates the bucket.
func (p *Platform) createBucket(region string) error {
	s := s3.New(session.New(aws.NewConfig().WithRegion(region)))
	n := p.getS3BucketName()

	log.WithField("name", n).Debug("creating s3 bucket")
	_, err := s.CreateBucket(&s3.CreateBucketInput{
		Bucket: &n,
	})

	return err
}

// deleteBucket deletes the bucket.
func (p *Platform) deleteBucket(region string) error {
	if err := p.emptyBucket(region); err != nil {
		return errors.Wrap(err, "emptying bucket")
	}

	s := s3.New(session.New(aws.NewConfig().WithRegion(region)))
	n := p.getS3BucketName()

	log.WithField("name", n).Debug("deleting s3 bucket")
	_, err := s.DeleteBucket(&s3.DeleteBucketInput{
		Bucket: &n,
	})

	return err
}

// emptyBucket empty the bucket.
func (p *Platform) emptyBucket(region string) error {
	s := s3.New(session.New(aws.NewConfig().WithRegion(region)))
	b := aws.String(p.getS3BucketName())

	params := &s3.ListObjectsInput{
		Bucket: b,
	}

	return s.ListObjectsPages(params, func(page *s3.ListObjectsOutput, lastPage bool) bool {
		for _, c := range page.Contents {
			ctx := log.WithField("key", *c.Key)

			ctx.Debug("deleting object")
			_, err := s.DeleteObject(&s3.DeleteObjectInput{
				Bucket: b,
				Key:    c.Key,
			})

			if err != nil {
				ctx.WithError(err).Warn("deleting object")
			}
		}

		return *page.IsTruncated
	})
}

// getAPI returns the API if present or nil.
func (p *Platform) getAPI(c *apigateway.APIGateway) (api *apigateway.RestApi, err error) {
	name := p.config.Name

	res, err := c.GetRestApis(&apigateway.GetRestApisInput{
		Limit: aws.Int64(500),
	})

	if err != nil {
		return nil, errors.Wrap(err, "fetching apis")
	}

	for _, a := range res.Items {
		if *a.Name == name {
			api = a
		}
	}

	return
}

// injectProxy injects the Go proxy.
func (p *Platform) injectProxy() error {
	log.Debugf("injecting proxy")

	if err := ioutil.WriteFile("main", bin.MustAsset("up-proxy"), 0777); err != nil {
		return errors.Wrap(err, "writing up-proxy")
	}

	if err := ioutil.WriteFile("byline.js", shim.MustAsset("byline.js"), 0755); err != nil {
		return errors.Wrap(err, "writing byline.js")
	}

	if err := ioutil.WriteFile("_proxy.js", shim.MustAsset("index.js"), 0755); err != nil {
		return errors.Wrap(err, "writing _proxy.js")
	}

	return nil
}

// removeProxy removes the Go proxy.
func (p *Platform) removeProxy() error {
	log.Debugf("removing proxy")
	os.Remove("main")
	os.Remove("_proxy.js")
	os.Remove("byline.js")
	return nil
}

// getS3Key returns a randomized s3 key.
func (p *Platform) getS3Key(stage string) string {
	return fmt.Sprintf("%s/%s/%s.zip", p.config.Name, stage, uniuri.New())
}

// getS3BucketName returns the s3 bucket name.
func (p *Platform) getS3BucketName() string {
	h := util.Md5(p.getAccountID())
	return fmt.Sprintf("up-deploys-%s-%s", h, p.config.Name)
}

// getAccountID returns the AWS account id derived from Lambda role,
// which is currently always present, implicitly or explicitly.
func (p *Platform) getAccountID() string {
	return strings.Split(p.config.Lambda.Role, ":")[4]
}

// isCreatingRole returns true if the role has not been created.
func isCreatingRole(err error) bool {
	return err != nil && strings.Contains(err.Error(), "role defined for the function cannot be assumed by Lambda")
}

// toEnv returns a lambda environment.
func toEnv(env config.Environment, stage string) *lambda.Environment {
	m := aws.StringMap(env)
	m["UP_STAGE"] = &stage
	return &lambda.Environment{
		Variables: m,
	}
}

// getCert returns the ARN if the cert is present.
func getCert(certs []*acm.CertificateSummary, domain string) string {
	for _, c := range certs {
		if *c.DomainName == domain {
			return *c.CertificateArn
		}
	}
	return ""
}
