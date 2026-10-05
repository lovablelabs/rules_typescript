export default {
  plugins: [
    {
      name: "force-declared-culori-optimization",
      config() {
        return { optimizeDeps: { include: ["culori"] } };
      },
    },
  ],
};
