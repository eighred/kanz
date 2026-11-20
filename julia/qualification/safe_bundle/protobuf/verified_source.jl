module VerifiedProtoBuf

using Pkg

const BASE_REVISION = "b3d83a7ce236ce44d8de6e688080069f176f4884"
const WORKER_TREE = "72afd74d71ca3bc51e6678fca28cd6f9b3f1085a"

function verify(package)
    root = realpath(package)
    git = `git -c safe.directory=$root --no-optional-locks -C $root`
    readchomp(`$git rev-parse HEAD`) == BASE_REVISION || error("ProtoBuf source revision differs")
    success(`$git diff --cached --quiet --no-ext-diff $WORKER_TREE --`) ||
        error("applied ProtoBuf source tree differs")
    # Hash the actual package contents using the pinned Pkg tree algorithm.
    # This also covers files omitted by Git's ignore rules or index stat cache.
    actual = bytes2hex(Pkg.GitTools.tree_hash(root))
    actual == WORKER_TREE || error("ProtoBuf package bytes differ")
    return actual
end

end
