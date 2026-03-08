#include <pthread.h>
#include <stdio.h>
#include <unistd.h>
#include <sys/syscall.h>

void* say_hi(void* arg) {
    printf("Thread %ld reporting in\n", syscall(SYS_gettid));
    return NULL;
}

int main() {
    pthread_t threads[10];
    for (int i = 0; i < 10; i++) {
        pthread_create(&threads[i], NULL, say_hi, NULL);
    }
    for (int i = 0; i < 10; i++) {
        pthread_join(threads[i], NULL);
    }
    return 0;
}
