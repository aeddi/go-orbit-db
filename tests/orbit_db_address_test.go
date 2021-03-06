package tests

import (
	"testing"

	"berty.tech/go-orbit-db/address"
	. "github.com/smartystreets/goconvey/convey"
)

func TestOrbitDbAddress(t *testing.T) {
	//.createInstance(ipfs, { directory: path.join(dbPath, '1') })
	Convey("orbit-db - OrbitDB Address", t, FailureHalts, func(c C) {
		c.Convey("Parse Address", FailureHalts, func(c C) {
			c.Convey("throws an error if address is empty", FailureHalts, func(c C) {
				result, err := address.Parse("")
				c.So(result, ShouldBeNil)
				c.So(err, ShouldNotBeNil)
				c.So(err.Error(), ShouldContainSubstring, "not a valid OrbitDB address")
			})

			c.Convey("parse address successfully", FailureHalts, func(c C) {
				refAddr := "/orbitdb/bafyreieecvmpthaoyasxzhnew2d25uaebwldeokea2wigyq5wr4dwiaimi/first-database"
				result, err := address.Parse(refAddr)
				c.So(err, ShouldBeNil)
				c.So(result, ShouldNotBeNil)

				c.So(result.GetRoot().String(), ShouldEqual, "bafyreieecvmpthaoyasxzhnew2d25uaebwldeokea2wigyq5wr4dwiaimi")
				c.So(result.GetPath(), ShouldEqual, "first-database")

				c.So(result.String(), ShouldStartWith, "/orbitdb")
				c.So(result.String(), ShouldContainSubstring, "bafy")
			})
		})

		c.Convey("isValid Address", FailureHalts, func(c C) {
			c.Convey("returns false for empty string", FailureHalts, func(c C) {
				err := address.IsValid("")
				c.So(err, ShouldNotBeNil)
			})

			c.Convey("validate address successfully", FailureHalts, func(c C) {
				testAddr := "/orbitdb/bafyreieecvmpthaoyasxzhnew2d25uaebwldeokea2wigyq5wr4dwiaimi/first-database"
				err := address.IsValid(testAddr)

				c.So(err, ShouldBeNil)
			})

			c.Convey("handle missing orbitdb prefix", FailureHalts, func(c C) {
				testAddr := "bafyreieecvmpthaoyasxzhnew2d25uaebwldeokea2wigyq5wr4dwiaimi/first-database"
				err := address.IsValid(testAddr)

				c.So(err, ShouldBeNil)
			})

			c.Convey("handle missing db address name", FailureHalts, func(c C) {
				testAddr := "bafyreieecvmpthaoyasxzhnew2d25uaebwldeokea2wigyq5wr4dwiaimi"
				err := address.IsValid(testAddr)

				c.So(err, ShouldBeNil)
			})

			c.Convey("handle invalid multihash", FailureHalts, func(c C) {
				testAddr := "/orbitdb/Qmdgwt7w4uBsw8LXduzCd18zfGXeTmBsiR8edQ1hSfzc/first-database"
				err := address.IsValid(testAddr)

				c.So(err, ShouldNotBeNil)
			})
		})

		TeardownNetwork()
	})
}
