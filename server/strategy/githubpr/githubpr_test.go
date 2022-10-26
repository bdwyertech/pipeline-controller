package githubpr_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/gittestserver"
	"github.com/fluxcd/pkg/runtime/logger"
	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v47/github"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/weaveworks/pipeline-controller/api/v1alpha1"
	"github.com/weaveworks/pipeline-controller/internal/testingutils"
	"github.com/weaveworks/pipeline-controller/server/strategy"
	"github.com/weaveworks/pipeline-controller/server/strategy/githubpr"
)

func initGitRepo(server *gittestserver.GitServer, fixture, branch, repositoryPath string) (*gogit.Repository, error) {
	fs := memfs.New()
	repo, err := gogit.Init(memory.NewStorage(), fs)
	if err != nil {
		return nil, err
	}

	err = commitFromFixture(repo, fixture)
	if err != nil {
		return nil, err
	}

	headRef, err := repo.Head()
	if err != nil {
		return nil, err
	}
	branchRef := plumbing.NewBranchReferenceName(branch)
	ref := plumbing.NewHashReference(branchRef, headRef.Hash())
	if err := repo.Storer.SetReference(ref); err != nil {
		return nil, err
	}

	if server.HTTPAddress() == "" {
		if err = server.StartHTTP(); err != nil {
			return nil, err
		}
		defer server.StopHTTP()
	}
	if _, err = repo.CreateRemote(&config.RemoteConfig{
		Name: gogit.DefaultRemoteName,
		URLs: []string{server.HTTPAddressWithCredentials() + repositoryPath},
	}); err != nil {
		return nil, err
	}

	if err = repo.Push(&gogit.PushOptions{
		RefSpecs: []config.RefSpec{"refs/heads/*:refs/heads/*"},
	}); err != nil {
		return nil, err
	}

	return repo, nil
}

func commitFromFixture(repo *gogit.Repository, fixture string) error {
	working, err := repo.Worktree()
	if err != nil {
		return err
	}
	fs := working.Filesystem

	if err = filepath.Walk(fixture, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fs.MkdirAll(fs.Join(path[len(fixture):]), info.Mode())
		}

		fileBytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		ff, err := fs.Create(path[len(fixture):])
		if err != nil {
			return err
		}
		defer ff.Close()

		_, err = ff.Write(fileBytes)
		return err
	}); err != nil {
		return err
	}

	_, err = working.Add(".")
	if err != nil {
		return err
	}

	if _, err = working.Commit("Fixtures from "+fixture, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Jane Doe",
			Email: "jane@example.com",
			When:  time.Now(),
		},
	}); err != nil {
		return err
	}

	return nil
}

func initTestTLS() ([]byte, []byte, []byte) {
	tlsPublicKey, err := os.ReadFile("testdata/certs/server.pem")
	if err != nil {
		panic(err)
	}
	tlsPrivateKey, err := os.ReadFile("testdata/certs/server-key.pem")
	if err != nil {
		panic(err)
	}
	tlsCA, err := os.ReadFile("testdata/certs/ca.pem")
	if err != nil {
		panic(err)
	}

	return tlsPublicKey, tlsPrivateKey, tlsCA
}

type mockGitHubClient struct {
	prs []*github.PullRequest
}

func (m *mockGitHubClient) PullRequests() githubpr.PullRequestClient {
	return m
}

func (m *mockGitHubClient) Create(ctx context.Context, owner string, repo string, pull *github.NewPullRequest) (*github.PullRequest, *github.Response, error) {
	newPR := &github.PullRequest{
		HTMLURL: pointer.String("location"),
	}
	m.prs = append(m.prs, newPR)
	return newPR, nil, nil
}

func (m *mockGitHubClient) List(ctx context.Context, owner string, repo string, opts *github.PullRequestListOptions) ([]*github.PullRequest, *github.Response, error) {
	return m.prs, nil, nil
}

func (m *mockGitHubClient) Edit(ctx context.Context, owner string, repo string, number int, pull *github.PullRequest) (*github.PullRequest, *github.Response, error) {
	panic("not implemented") // TODO: Implement
}

func mockGitHubClientFactory() func(c *http.Client) githubpr.GitHubClient {
	return func(c *http.Client) githubpr.GitHubClient {
		return &mockGitHubClient{}
	}
}

func TestHandles(t *testing.T) {
	tests := []struct {
		name     string
		in       v1alpha1.Promotion
		expected bool
	}{
		{
			"empty promotion",
			v1alpha1.Promotion{},
			false,
		},
		{
			"nil PullRequestPromotion",
			v1alpha1.Promotion{PullRequest: nil},
			false,
		},
		{
			"empty PullRequestPromotion",
			v1alpha1.Promotion{PullRequest: &v1alpha1.PullRequestPromotion{}},
			true,
		},
	}

	strat, err := githubpr.NewGitHubPR(nil, logger.NewLogger(logger.Options{}))
	if err != nil {
		t.Fatalf("unable to create GitHub promotion strategy: %s", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := testingutils.NewGomegaWithT(t)
			g.Expect(strat.Handles(tt.in)).To(Equal(tt.expected))
		})
	}
}

func TestPromote(t *testing.T) {
	tlsPublicKey, tlsPrivateKey, tlsCA := initTestTLS()

	type gitServerConfig struct {
		repoFixtureDir string
		username       string
		password       string
		publicKey      []byte
		privateKey     []byte
		ca             []byte
	}
	tests := []struct {
		name       string
		promSpec   v1alpha1.Promotion
		promotion  strategy.Promotion
		apiObjects []client.Object
		server     *gitServerConfig
		err        error
		errPattern string
	}{
		{
			"nil PullRequest spec",
			v1alpha1.Promotion{
				PullRequest: nil,
			},
			strategy.Promotion{},
			nil,
			nil,
			githubpr.ErrSpecIsNil,
			"",
		},
		{
			"Secret not specified",
			v1alpha1.Promotion{
				PullRequest: &v1alpha1.PullRequestPromotion{},
			},
			strategy.Promotion{},
			nil,
			nil,
			nil,
			"failed to fetch credentials: failed to fetch Secret: secrets \"\" not found",
		},
		{
			"no repo URL specified",
			v1alpha1.Promotion{
				PullRequest: &v1alpha1.PullRequestPromotion{
					SecretRef: meta.LocalObjectReference{
						Name: "repo-credentials",
					},
				},
			},
			strategy.Promotion{
				PipelineNamespace: "foobar",
			},
			[]client.Object{
				&corev1.Secret{
					ObjectMeta: v1.ObjectMeta{
						Namespace: "foobar",
						Name:      "repo-credentials",
					},
				},
			},
			nil,
			nil,
			"failed to clone repo: failed configuring auth opts for repo URL \"\": no transport type set",
		},
		{
			"repo URL is invalid",
			v1alpha1.Promotion{
				PullRequest: &v1alpha1.PullRequestPromotion{
					URL: "https://example.org",
					SecretRef: meta.LocalObjectReference{
						Name: "repo-credentials",
					},
				},
			},
			strategy.Promotion{
				PipelineNamespace: "foobar",
			},
			[]client.Object{
				&corev1.Secret{
					ObjectMeta: v1.ObjectMeta{
						Namespace: "foobar",
						Name:      "repo-credentials",
					},
				},
			},
			nil,
			nil,
			"failed to clone repo: failed cloning repository: unable to clone: repository not found: git repository: 'https://example.org'",
		},
		{
			"no GitHub token",
			v1alpha1.Promotion{
				PullRequest: &v1alpha1.PullRequestPromotion{
					URL: "to-be-filled-in-by-test-code",
					SecretRef: meta.LocalObjectReference{
						Name: "repo-credentials",
					},
				},
			},
			strategy.Promotion{
				PipelineNamespace: "foo",
				PipelineName:      "bar",
			},
			[]client.Object{
				&corev1.Secret{
					ObjectMeta: v1.ObjectMeta{
						Namespace: "foo",
						Name:      "repo-credentials",
					},
				},
			},
			&gitServerConfig{
				repoFixtureDir: "testdata/git/repository",
			},
			nil,
			"failed to create PR: GitHub token is empty",
		},
		{
			"missing/invalid credentials",
			v1alpha1.Promotion{
				PullRequest: &v1alpha1.PullRequestPromotion{
					URL: "to-be-filled-in-by-test-code",
					SecretRef: meta.LocalObjectReference{
						Name: "repo-credentials",
					},
				},
			},
			strategy.Promotion{
				PipelineNamespace: "foo",
				PipelineName:      "bar",
			},
			[]client.Object{
				&corev1.Secret{
					ObjectMeta: v1.ObjectMeta{
						Namespace: "foo",
						Name:      "repo-credentials",
					},
				},
			},
			&gitServerConfig{
				repoFixtureDir: "testdata/git/repository",
				username:       "user",
				password:       "pass",
			},
			nil,
			"failed to clone repo: failed cloning repository: unable to clone '.*': authentication required",
		},
		{
			"HTTP scheme not supported",
			v1alpha1.Promotion{
				PullRequest: &v1alpha1.PullRequestPromotion{
					URL: "to-be-filled-in-by-test-code",
					SecretRef: meta.LocalObjectReference{
						Name: "repo-credentials",
					},
				},
			},
			strategy.Promotion{
				PipelineNamespace: "foo",
				PipelineName:      "bar",
			},
			[]client.Object{
				&corev1.Secret{
					ObjectMeta: v1.ObjectMeta{
						Namespace: "foo",
						Name:      "repo-credentials",
					},
					Data: map[string][]byte{
						"token":    []byte("token"),
						"username": []byte("user"),
						"password": []byte("pass"),
					},
				},
			},
			&gitServerConfig{
				repoFixtureDir: "testdata/git/repository",
				username:       "user",
				password:       "pass",
			},
			nil,
			"failed to create PR: failed parsing GitHub URL: unsupported URL scheme, only HTTPS supported",
		},
		{
			"happy path with auth",
			v1alpha1.Promotion{
				PullRequest: &v1alpha1.PullRequestPromotion{
					URL: "to-be-filled-in-by-test-code",
					SecretRef: meta.LocalObjectReference{
						Name: "repo-credentials",
					},
				},
			},
			strategy.Promotion{
				PipelineNamespace: "foo",
				PipelineName:      "bar",
			},
			[]client.Object{
				&corev1.Secret{
					ObjectMeta: v1.ObjectMeta{
						Namespace: "foo",
						Name:      "repo-credentials",
					},
					Data: map[string][]byte{
						"token":    []byte("token"),
						"username": []byte("user"),
						"password": []byte("pass"),
						"caFile":   tlsCA,
					},
				},
			},
			&gitServerConfig{
				repoFixtureDir: "testdata/git/repository",
				username:       "user",
				password:       "pass",
				publicKey:      tlsPublicKey,
				privateKey:     tlsPrivateKey,
				ca:             tlsCA,
			},
			nil,
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := testingutils.NewGomegaWithT(t)
			fc := fake.NewClientBuilder().WithObjects(tt.apiObjects...).Build()
			mockCF := mockGitHubClientFactory()
			strat, err := githubpr.NewGitHubPR(fc, logger.NewLogger(logger.Options{}), githubpr.GitHubClientFactory(mockCF))
			if err != nil {
				t.Fatalf("unable to create GitHub promotion strategy: %s", err)
			}
			if tt.server != nil {
				server, err := gittestserver.NewTempGitServer()
				g.Expect(err).NotTo(HaveOccurred())
				t.Cleanup(func() {
					os.RemoveAll(server.Root())
				})
				server.AutoCreate()
				repoPath := "/org/test.git"
				_, err = initGitRepo(server, tt.server.repoFixtureDir, v1alpha1.DefaultBranch, repoPath)
				g.Expect(err).NotTo(HaveOccurred())
				if tt.server.username != "" {
					server.Auth(tt.server.username, tt.server.password)
				}
				if tt.server.privateKey != nil {
					g.Expect(server.StartHTTPS(tt.server.publicKey, tt.server.privateKey, tt.server.ca, "example.org")).To(Succeed())
				} else {
					g.Expect(server.StartHTTP()).To(Succeed())
				}
				t.Cleanup(func() {
					server.StopHTTP()
				})
				tt.promSpec.PullRequest.URL = server.HTTPAddress() + repoPath
			}
			res, err := strat.Promote(context.Background(), tt.promSpec, tt.promotion)
			if tt.err != nil {
				g.Expect(err).To(Equal(tt.err))
			} else if tt.errPattern != "" {
				g.Expect(err).To(MatchError(MatchRegexp(tt.errPattern)))
			} else {
				g.Expect(err).To(BeNil())
				g.Expect(res).NotTo(BeNil())
			}
		})
	}
}