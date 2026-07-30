#include <cstdio>
#include <exception>
#include <functional>
#include <pthread.h>

extern "C" void handle(int fd);

namespace {

// RAII guard over a plain pthread_mutex_t. The mutex itself has to stay a
// pthread_mutex_t (not a std::mutex) because it is declared and used from
// main.c, which is compiled as C.
class PthreadMutexGuard {
public:
    explicit PthreadMutexGuard(pthread_mutex_t *m) : m_(m) {
        pthread_mutex_lock(m_);
    }
    ~PthreadMutexGuard() { pthread_mutex_unlock(m_); }

    PthreadMutexGuard(const PthreadMutexGuard &) = delete;
    PthreadMutexGuard &operator=(const PthreadMutexGuard &) = delete;

private:
    pthread_mutex_t *m_;
};

} // namespace

// Runs fn(arg) with *m held and releases it on every exit path, including a
// C++ exception thrown out of fn.
//
// This has to live in the C++ translation unit. main.c is compiled as C
// without -fexceptions, so it gets no landing pads: when a libandroidappmusic
// call throws, the unwinder walks straight through the C frames and every
// pthread_mutex_unlock written there is skipped, leaking the mutex for the
// life of the process. Owning the lock in this frame is what makes the
// release unconditional -- the guard's destructor runs during unwinding.
//
// The exception is deliberately not caught here, so it still reaches
// handle_cpp below and the connection still fails the way it always has.
// Only the mutex leak is fixed.
extern "C" void run_with_mutex(pthread_mutex_t *m, void (*fn)(void *),
                               void *arg) {
    PthreadMutexGuard guard(m);
    fn(arg);
}

extern "C" uint8_t handle_cpp(int fd) {
    try {
        handle(fd);
        return 1;
    } catch (const std::exception &e) {
        fprintf(stderr, "[!] catched an exception: %s\n", e.what());
        return 0;
    }
}

static void endLeaseCb(int const &c) {
    fprintf(stderr, "[.] end lease code %d - ignoring exit in concurrent mode\n", c);
}

static void pbErrCb(void *) {
    fprintf(stderr, "[.] playback error - ignoring exit in concurrent mode\n");
}

extern "C" std::function<void (int const&)> endLeaseCallback(endLeaseCb);
extern "C" std::function<void (void *)> pbErrCallback(pbErrCb);