package lock

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const testRedisKey = "__bsm_redis_lock_unit_test__"

var _ = Describe("Lock", func() {
	var subject *Lock

	var newLock = func() *Lock {
		return NewLock(redisClient, testRedisKey, &Options{
			WaitTimeout: 100 * time.Millisecond,
			LockTimeout: time.Second,
		})
	}

	BeforeEach(func() {
		subject = newLock()
		Expect(subject.IsLocked()).To(BeFalse())
	})

	AfterEach(func() {
		Expect(redisClient.Del(testRedisKey).Err()).NotTo(HaveOccurred())
	})

	It("should normalize options", func() {
		lock := NewLock(redisClient, testRedisKey, &Options{
			RetriesCount: -1,
			LockTimeout:  -1,
			WaitRetry:    -1,
			WaitTimeout:  -1,
		})
		Expect(lock.opts.RetriesCount).To(Equal(0))
		Expect(lock.opts.LockTimeout).To(Equal(minLockTimeout))
		Expect(lock.opts.WaitRetry).To(Equal(minWaitRetry))
		Expect(lock.opts.WaitTimeout).To(Equal(time.Duration(0)))
	})

	It("should fail with `can't get lock`", func() {
		lock := newLock()
		lock.Lock()
		defer lock.Unlock()
		_, err := ObtainLock(redisClient, testRedisKey, nil)
		Expect(err).To(Equal(ErrCanntGetLock))
	})

	It("should o btain through short-cut", func() {
		lock, err := ObtainLock(redisClient, testRedisKey, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(lock).To(BeAssignableToTypeOf(subject))
	})

	It("should obtain fresh locks", func() {
		ok, err := subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(subject.IsLocked()).To(BeTrue())

		val := redisClient.Get(testRedisKey).Val()
		Expect(val).To(HaveLen(24))

		ttl := redisClient.PTTL(testRedisKey).Val()
		Expect(ttl).To(BeNumerically("~", time.Second, 10*time.Millisecond))
	})

	It("should wait for expiring locks if WaitTimeout is set", func() {
		Expect(redisClient.Set(testRedisKey, "ABCD", 0).Err()).NotTo(HaveOccurred())
		Expect(redisClient.PExpire(testRedisKey, 50*time.Millisecond).Err()).NotTo(HaveOccurred())

		ok, err := subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(subject.IsLocked()).To(BeTrue())

		val := redisClient.Get(testRedisKey).Val()
		Expect(val).To(HaveLen(24))
		Expect(subject.token).To(Equal(val))

		ttl := redisClient.PTTL(testRedisKey).Val()
		Expect(ttl).To(BeNumerically("~", time.Second, 10*time.Millisecond))
	})

	It("should wait until WaitTimeout is reached, then give up", func() {
		Expect(redisClient.Set(testRedisKey, "ABCD", 0).Err()).NotTo(HaveOccurred())
		Expect(redisClient.PExpire(testRedisKey, 150*time.Millisecond).Err()).NotTo(HaveOccurred())

		ok, err := subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(subject.IsLocked()).To(BeFalse())
		Expect(subject.token).To(Equal(""))

		val := redisClient.Get(testRedisKey).Val()
		Expect(val).To(Equal("ABCD"))

		ttl := redisClient.PTTL(testRedisKey).Val()
		Expect(ttl).To(BeNumerically("~", 50*time.Millisecond, 10*time.Millisecond))
	})

	It("should not wait for expiring locks if WaitTimeout is not set", func() {
		Expect(redisClient.Set(testRedisKey, "ABCD", 0).Err()).NotTo(HaveOccurred())
		Expect(redisClient.PExpire(testRedisKey, 150*time.Millisecond).Err()).NotTo(HaveOccurred())
		subject.opts.WaitTimeout = 0

		ok, err := subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(subject.IsLocked()).To(BeFalse())

		ttl := redisClient.PTTL(testRedisKey).Val()
		Expect(ttl).To(BeNumerically("~", 150*time.Millisecond, 10*time.Millisecond))
	})

	It("should release own locks", func() {
		ok, err := subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(subject.IsLocked()).To(BeTrue())

		Expect(subject.Unlock()).NotTo(HaveOccurred())
		Expect(subject.token).To(Equal(""))
		Expect(subject.IsLocked()).To(BeFalse())
		Expect(redisClient.Get(testRedisKey).Err()).To(Equal(redis.Nil))
	})

	It("should not release someone else's locks", func() {
		Expect(redisClient.Set(testRedisKey, "ABCD", 0).Err()).NotTo(HaveOccurred())
		Expect(subject.IsLocked()).To(BeFalse())

		Expect(subject.Unlock()).NotTo(HaveOccurred())
		Expect(subject.token).To(Equal(""))
		Expect(subject.IsLocked()).To(BeFalse())
		Expect(redisClient.Get(testRedisKey).Val()).To(Equal("ABCD"))
	})

	It("should refresh locks", func() {
		ok, err := subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(subject.IsLocked()).To(BeTrue())

		time.Sleep(50 * time.Millisecond)
		ttl := redisClient.PTTL(testRedisKey).Val()
		Expect(ttl).To(BeNumerically("~", 950*time.Millisecond, 10*time.Millisecond))

		ok, err = subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(subject.IsLocked()).To(BeTrue())
		ttl = redisClient.PTTL(testRedisKey).Val()
		Expect(ttl).To(BeNumerically("~", time.Second, 10*time.Millisecond))
	})

	It("should re-create expired locks on refresh", func() {
		ok, err := subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(subject.IsLocked()).To(BeTrue())
		token := subject.token

		Expect(redisClient.Del(testRedisKey).Err()).NotTo(HaveOccurred())

		ok, err = subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(subject.IsLocked()).To(BeTrue())
		Expect(subject.token).NotTo(Equal(token))
		ttl := redisClient.PTTL(testRedisKey).Val()
		Expect(ttl).To(BeNumerically("~", time.Second, 10*time.Millisecond))
	})

	It("should not re-capture expired locks acquiredby someone else", func() {
		ok, err := subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(subject.IsLocked()).To(BeTrue())
		Expect(redisClient.Set(testRedisKey, "ABCD", 0).Err()).NotTo(HaveOccurred())

		ok, err = subject.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(subject.IsLocked()).To(BeFalse())
	})

	It("should prevent multiple locks (fuzzing)", func() {
		res := int32(0)
		wg := new(sync.WaitGroup)
		for i := 0; i < 1000; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				lock := newLock()
				wait := rand.Int63n(int64(50 * time.Millisecond))
				time.Sleep(time.Duration(wait))

				ok, err := lock.Lock()
				if err != nil {
					atomic.AddInt32(&res, 100)
					return
				} else if !ok {
					return
				}
				atomic.AddInt32(&res, 1)
			}()
		}
		wg.Wait()
		Expect(res).To(Equal(int32(1)))
	})

	It("should run with locks and prevent fuzzing", func() {
		res := int32(0)
		wg := new(sync.WaitGroup)

		RunWithLock(redisClient, testRedisKey, func() error {
			for i := 0; i < 1000; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()

					lock := newLock()
					wait := rand.Int63n(int64(50 * time.Millisecond))
					time.Sleep(time.Duration(wait))

					ok, err := lock.Lock()
					if err != nil {
						atomic.AddInt32(&res, 100)
						return
					} else if !ok {
						return
					}
					atomic.AddInt32(&res, 1)
				}()
			}

			wg.Wait()

			return nil
		}, nil)

		Expect(res).To(Equal(int32(0)))
	})

	It("should wait for locks", func() {
		res := 0
		wg := new(sync.WaitGroup)
		wg.Add(1)
		count := 6
		timeout := 50 * time.Millisecond

		go func() {
			RunWithLock(redisClient, testRedisKey, func() error {
				res++
				return nil
			}, &Options{RetriesCount: count})
			wg.Done()
		}()

		var err = RunWithLock(redisClient, testRedisKey, func() error {
			res++
			time.Sleep(timeout)
			return nil
		}, nil)

		wg.Wait()

		Expect(err).To(BeNil())
		Expect(res).To(Equal(2))
	})

	It("should not wait for locks", func() {
		res := 0
		wg := new(sync.WaitGroup)
		wg.Add(1)
		count := 1
		timeout := 20 * time.Millisecond

		go func() {
			RunWithLock(redisClient, testRedisKey, func() error {
				res++
				return nil
			}, &Options{RetriesCount: count})
			wg.Done()
		}()

		var err = RunWithLock(redisClient, testRedisKey, func() error {
			res++
			time.Sleep(timeout)
			return nil
		}, nil)

		wg.Wait()

		Expect(err).To(BeNil())
		Expect(res).To(Equal(1))
	})

})

func TestSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	AfterEach(func() {
		Expect(redisClient.Del(testRedisKey).Err()).NotTo(HaveOccurred())
	})
	RunSpecs(t, "redis-lock")
}

var redisClient *redis.Client

var _ = BeforeSuite(func() {
	redisClient = redis.NewClient(&redis.Options{
		Network: "tcp",
		Addr:    "127.0.0.1:6379", DB: 9,
	})
	Expect(redisClient.Ping().Err()).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	redisClient.Close()
})
