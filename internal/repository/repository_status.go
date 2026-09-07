package repository

import (
	"github.com/nlewo/comin/internal/types"
	pb "github.com/nlewo/comin/pkg/protobuf"
)

func NewGitRepositoryStatus(config types.GitConfig, mainCommitId string) *pb.GitRepositoryStatus {
	r := &pb.GitRepositoryStatus{
		MainCommitId: mainCommitId,
	}
	r.Remotes = make([]*pb.Remote, len(config.Remotes))
	for i, remote := range config.Remotes {
		r.Remotes[i] = &pb.Remote{
			Name: remote.Name,

			Url: remote.URL,
			Main: &pb.Branch{
				Name: remote.Branches.Main.Name,
			},
			Testing: &pb.Branch{
				Name: remote.Branches.Testing.Name,
			},
		}
	}
	return r
}

// func (r GitRepositoryStatus) IsTesting() bool {
// 	return r.SelectedBranchIsTesting
// }

func GetRemote(r *pb.GitRepositoryStatus, remoteName string) *pb.Remote {
	for _, remote := range r.Remotes {
		if remote.Name == remoteName {
			return remote
		}
	}
	return nil
}
